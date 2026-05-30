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

	"github.com/imprezahost/impreza-devkit/sdk-go/config"
)

// Client is the entry point for every API call. Construct one per
// invocation via New(); it carries the auth-injecting transport and
// the resolved base URL.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a Client wired to the given context, using the standard
// API-key auth realm (X-API-Key + X-API-Secret). Uses the retry
// transport on top of auth-injecting transport. SOCKS5 proxy is
// attached when ctx.UseTor or ctx.Proxy is set.
//
// Use NewAgent instead for the impreza-agent daemon, which speaks the
// X-Agent-Id + X-Agent-Secret realm at the `/v1/agent/*` endpoints.
func New(ctx config.Context) (*Client, error) {
	base, err := buildTransport(ctx)
	if err != nil {
		return nil, err
	}
	authed := &authTransport{
		base:      base,
		keyHeader: headerAPIKey,
		secHeader: headerAPISecret,
		keyValue:  ctx.Key,
		secValue:  ctx.Secret,
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

// AgentOptions configures the agent-realm Client built by NewAgent.
type AgentOptions struct {
	// AgentID is the per-agent identifier issued at bootstrap (agt_...).
	AgentID string
	// AgentSecret is the per-agent secret issued at bootstrap. Shown
	// ONCE to the agent and persisted to /etc/impreza-agent/credentials.toml.
	AgentSecret string
	// BaseURL overrides the default control-plane URL when non-empty.
	// Used by staging / on-prem deployments.
	BaseURL string
	// UseTor routes the agent's outbound traffic through the default
	// Tor SOCKS port. Reports / polls / deploy results all flow via
	// Tor when set.
	UseTor bool
	// Proxy explicitly sets a SOCKS5 URL (overrides UseTor when both
	// are set).
	Proxy string
	// Timeout overrides the default 180s HTTP timeout. The agent's
	// long-poll bumps this internally per request — this default is
	// the safety net for misbehaving operations.
	Timeout time.Duration
}

// NewAgent returns a Client wired to the agent auth realm (X-Agent-Id +
// X-Agent-Secret). All other behaviors — retry, Tor, envelope
// decoding — are shared with the standard Client. The same `c.Get`,
// `c.Post`, etc. work; the resource methods that target `/v1/agent/*`
// are documented in `agent.go`.
func NewAgent(opts AgentOptions) (*Client, error) {
	base, err := buildTransport(config.Context{
		UseTor: opts.UseTor,
		Proxy:  opts.Proxy,
	})
	if err != nil {
		return nil, err
	}
	authed := &authTransport{
		base:      base,
		keyHeader: headerAgentID,
		secHeader: headerAgentSecret,
		keyValue:  opts.AgentID,
		secValue:  opts.AgentSecret,
	}
	retrying := &retryTransport{base: authed}

	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 180 * time.Second
	}

	return &Client{
		BaseURL: baseURL,
		HTTP: &http.Client{
			Transport: retrying,
			Timeout:   timeout,
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

// GetRaw performs an authenticated GET and returns the raw response
// body as bytes. Skips the JSON envelope decode — useful for binary
// blobs (tarballs, archives, image streams). The caller is responsible
// for interpreting Content-Type. Non-2xx still surfaces as a typed
// error (best-effort: tries to decode the envelope from the body; if
// it's not JSON, returns mapStatus with empty code/message).
//
// Used by the impreza-agent's docker executor to download Phase 12
// custom-deploy build contexts from
// `GET /v1/agent/custom-deploy-contexts/{id}`.
func (c *Client) GetRaw(ctx context.Context, path string, query url.Values) ([]byte, error) {
	u, err := url.Parse(c.BaseURL + path)
	if err != nil {
		return nil, fmt.Errorf("build URL %s%s: %w", c.BaseURL, path, err)
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "*/*")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET %s: %w", u.Path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}
	// Try to surface the JSON error envelope if the server sent one.
	if isJSON(resp.Header.Get("Content-Type")) {
		var env envelope
		if err := json.Unmarshal(body, &env); err == nil && env.Error != nil {
			return nil, mapStatus(resp.StatusCode, env.Error.Code, env.Error.Message, requestIDFromHeader(resp))
		}
	}
	return nil, mapStatus(resp.StatusCode, "", "", requestIDFromHeader(resp))
}

// PostRaw performs an authenticated POST with an arbitrary binary body
// (the caller-set contentType MUST match the bytes — application/gzip
// for a tarball, application/octet-stream for generic). The response
// is decoded as a JSON envelope into target, matching the rest of the
// SDK contract. Used by Phase 12 custom-deploy context upload
// (`POST /v1/platform/deployments/custom/contexts` accepts a gzip
// tarball as the raw body).
func (c *Client) PostRaw(ctx context.Context, path, contentType string, body []byte, target any) error {
	u, err := url.Parse(c.BaseURL + path)
	if err != nil {
		return fmt.Errorf("build URL %s%s: %w", c.BaseURL, path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.ContentLength = int64(len(body))

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP POST %s: %w", u.Path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if !isJSON(resp.Header.Get("Content-Type")) {
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

	// 2xx with explicitly no body — handled before the Content-Type
	// check because the server doesn't have to set Content-Type on
	// 204 No Content (and several endpoints don't). DELETE and the
	// agent's `report` / `deploy-result` / `logs` endpoints return
	// 204 on success.
	if resp.StatusCode == http.StatusNoContent {
		return nil
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
