// Package client is the Impreza Host API client used by every resource
// command. Hand-written to match the OpenAPI 3.1 contract; co-evolved
// with the openapi.yaml spec.
//
// Phase 7.2 ships the read surface (account / catalog / domain / vps
// list / show / status / invoice / key whoami). 7.3+ adds the write
// verbs on top of the same Client + auth machinery.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/imprezahost/impreza-devkit/cli-go/internal/config"
)

// Client is the entry point for every API call. Construct one per
// invocation via New(); it carries the auth-injecting transport and
// the resolved base URL.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a Client wired to the given context. Uses the retry
// transport on top of auth-injecting transport. SOCKS5 proxy is
// attached when ctx.UseTor or ctx.Proxy is set.
func New(ctx config.Context) (*Client, error) {
	base, err := buildTransport(ctx)
	if err != nil {
		return nil, err
	}
	authed := &authTransport{
		base:   base,
		key:    ctx.Key,
		secret: ctx.Secret,
	}
	retrying := &retryTransport{base: authed}

	return &Client{
		BaseURL: BaseURL(ctx),
		HTTP: &http.Client{
			Transport: retrying,
			// Cloud upstream operations (boot-order, resize, image
			// create) sometimes take 60-120s to return; Proxmox reads
			// are sub-second. 180s is the upper bound that still
			// catches a stuck CI / network failure quickly without
			// breaking valid slow operations.
			Timeout: 180 * time.Second,
		},
	}, nil
}

// envelope mirrors the canonical wrapper every endpoint returns:
//
//	{"success": true,  "data": <T>, "meta": {...}}
//	{"success": false, "error": {...}, "meta": {...}}
//
// The generic `data` payload is decoded into the caller-supplied target
// after the success/error branch is determined.
type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   *apiErrorBody   `json:"error,omitempty"`
	Meta    *apiMeta        `json:"meta,omitempty"`
}

type apiErrorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type apiMeta struct {
	RequestID string `json:"request_id,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

// Get performs an authenticated GET to path (relative to BaseURL),
// applying query if non-nil. Decodes the response envelope's `data`
// field into target. Returns a typed error from errors.go on non-2xx.
func (c *Client) Get(ctx context.Context, path string, query url.Values, target any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, target)
}

// Post performs an authenticated POST with a JSON body.
func (c *Client) Post(ctx context.Context, path string, body any, target any) error {
	return c.do(ctx, http.MethodPost, path, nil, body, target)
}

// PostWithHeaders is Post + arbitrary request headers. Used by endpoints
// that demand an out-of-band confirmation header (e.g. the dedicated-server
// reinstall route, which requires X-Impreza-Confirm: WIPE in addition to
// the body confirmation flag).
func (c *Client) PostWithHeaders(
	ctx context.Context,
	path string,
	body any,
	headers map[string]string,
	target any,
) error {
	return c.doWithHeaders(ctx, http.MethodPost, path, nil, body, headers, target)
}

// Put performs an authenticated PUT with a JSON body.
func (c *Client) Put(ctx context.Context, path string, body any, target any) error {
	return c.do(ctx, http.MethodPut, path, nil, body, target)
}

// Delete performs an authenticated DELETE. Optional body for endpoints
// that identify the resource by a content tuple instead of a URL id
// (the DNS endpoint matches on type+host+value, for example). Most
// DELETE endpoints return 204 No Content or {success: true, data: null}.
func (c *Client) Delete(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodDelete, path, nil, body, nil)
}

// do handles the full request/response cycle: build URL, encode body,
// execute, decode envelope, dispatch error or success branch.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, target any) error {
	return c.doWithHeaders(ctx, method, path, query, body, nil, target)
}

// doWithHeaders is the same as do but lets callers attach extra request
// headers (Accept and Content-Type are still set automatically).
func (c *Client) doWithHeaders(ctx context.Context, method, path string, query url.Values, body any, headers map[string]string, target any) error {
	u, err := url.Parse(c.BaseURL + path)
	if err != nil {
		return fmt.Errorf("build URL %s%s: %w", c.BaseURL, path, err)
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}

	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP %s %s: %w", method, u.Path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	// On unexpected non-JSON 5xx (HTML error pages from the WAF, etc.),
	// surface the status as-is rather than trying to decode garbage.
	if !isJSON(resp.Header.Get("Content-Type")) {
		if resp.StatusCode == http.StatusOK {
			// 200 OK with non-JSON: shouldn't happen, but if it does,
			// the server is broken.
			return fmt.Errorf("unexpected non-JSON 200 response (Content-Type=%q)",
				resp.Header.Get("Content-Type"))
		}
		return mapStatus(resp.StatusCode, "", "", requestIDFromHeader(resp))
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode envelope: %w (raw=%q)", err, string(raw))
	}

	requestID := ""
	if env.Meta != nil {
		requestID = env.Meta.RequestID
	}
	if requestID == "" {
		requestID = requestIDFromHeader(resp)
	}

	if !env.Success || env.Error != nil {
		code := ""
		msg := ""
		if env.Error != nil {
			code = env.Error.Code
			msg = env.Error.Message
		}
		return mapStatus(resp.StatusCode, code, msg, requestID)
	}

	if target != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, target); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
	}
	return nil
}

func isJSON(contentType string) bool {
	if contentType == "" {
		return false
	}
	for _, prefix := range []string{"application/json", "application/problem+json"} {
		if len(contentType) >= len(prefix) && contentType[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func requestIDFromHeader(r *http.Response) string {
	for _, h := range []string{"X-Request-ID", "X-Request-Id", "Request-Id"} {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	return ""
}
