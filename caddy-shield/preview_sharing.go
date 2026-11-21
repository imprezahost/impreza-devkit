package caddyshield

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	sdkconfig "github.com/imprezahost/impreza-devkit/sdk-go/config"
)

const PreviewSharingIngressProtocol = "preview-sharing-ingress-v1"

func init() {
	caddy.RegisterModule(&PreviewSharingMiddleware{})
	httpcaddyfile.RegisterHandlerDirective("preview_sharing", func(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
		m := new(PreviewSharingMiddleware)
		e := m.UnmarshalCaddyfile(h.Dispenser)
		return m, e
	})
}

// PreviewSharingMiddleware is a separate, read-only onion origin. It does not
// change the protected preview's original route. No positive policy cache and
// no visitor metadata is sent to the control plane. JSON configuration only;
// poll orchestration/portal availability are intentionally a later increment.
// Credential comes in Basic auth, NEVER in a URL, cookie or route config.
type PreviewSharingMiddleware struct {
	Journal      string `json:"journal"`
	GrantID      string `json:"grant_id"`
	DeploymentID string `json:"deployment_id"`
	Generation   int    `json:"generation"`
	Destination  string `json:"destination"`
	ExpiresAt    int64  `json:"expires_at"`
	// Values resolve existing agent env placeholders during provisioning.
	APIURL           string `json:"api_url"`
	AuthorizedOrigin string `json:"authorized_origin,omitempty"`
	AgentID          string `json:"agent_id"`
	AgentSecret      string `json:"agent_secret"`
	client           *http.Client
}

func (*PreviewSharingMiddleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.handlers.preview_sharing", New: func() caddy.Module { return new(PreviewSharingMiddleware) }}
}

var sharingGrantID = regexp.MustCompile(`^psg_[a-f0-9]{32}$`)
var sharingDeploymentID = regexp.MustCompile(`^dpl_[a-zA-Z0-9_-]{1,28}$`)
var sharingOnion = regexp.MustCompile(`^[a-z2-7]{56}\.onion$`)
var sharingCredential = regexp.MustCompile(`^[a-zA-Z0-9_-]{43}$`)

func (m *PreviewSharingMiddleware) Provision(_ caddy.Context) error {
	if runtime.GOOS != "linux" {
		return errors.New("preview sharing ingress requires Linux")
	}
	replacer := caddy.NewReplacer()
	m.APIURL = replacer.ReplaceAll(m.APIURL, "")
	m.AuthorizedOrigin = replacer.ReplaceAll(m.AuthorizedOrigin, "")
	m.AgentID = replacer.ReplaceAll(m.AgentID, "")
	m.AgentSecret = replacer.ReplaceAll(m.AgentSecret, "")
	if err := m.Validate(); err != nil {
		return err
	}
	m.client = &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return nil
}

func (m *PreviewSharingMiddleware) Validate() error {
	u, e := url.Parse(m.APIURL)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") ||
		sdkconfig.AllowCredentialBase(m.APIURL, m.AuthorizedOrigin) != nil ||
		!sharingGrantID.MatchString(m.GrantID) || !sharingDeploymentID.MatchString(m.DeploymentID) || !sharingOnion.MatchString(m.Destination) ||
		m.Generation != 1 || m.ExpiresAt <= 0 || m.Journal != "/config/preview-sharing/"+m.GrantID+".json" || m.AgentID == "" || m.AgentSecret == "" || strings.ContainsAny(m.AgentID+m.AgentSecret, "\r\n") {
		return errors.New("invalid preview sharing policy")
	}
	return nil
}

type sharingPolicy struct {
	State        string `json:"state"`
	Protocol     string `json:"protocol"`
	GrantID      string `json:"grant_id"`
	DeploymentID string `json:"deployment_id"`
	Generation   int    `json:"generation"`
	Destination  string `json:"destination"`
	ExpiresAt    int64  `json:"expires_at"`
	Allowed      bool   `json:"allowed"`
}

func (m *PreviewSharingMiddleware) authorize(ctx context.Context, credential string) bool {
	digest := sha256.Sum256([]byte(credential))
	payload, _ := json.Marshal(map[string]any{"protocol": PreviewSharingIngressProtocol, "grant_id": m.GrantID, "deployment_id": m.DeploymentID,
		"generation": m.Generation, "destination": m.Destination, "token_hash": hex.EncodeToString(digest[:])})
	if err := sdkconfig.AllowCredentialBase(m.APIURL, m.AuthorizedOrigin); err != nil || m.client == nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.APIURL, "/")+"/v1/agent/preview-sharing/authorize", bytes.NewReader(payload))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Id", m.AgentID)
	req.Header.Set("X-Agent-Secret", m.AgentSecret)
	// No original URL, headers, visitor address, referrer or credential.
	res, err := m.client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return false
	}
	var envelope struct {
		Success bool          `json:"success"`
		Data    sharingPolicy `json:"data"`
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4097))
	if err != nil || len(raw) > 4096 || json.Unmarshal(raw, &envelope) != nil {
		return false
	}
	q := envelope.Data
	return envelope.Success && q.State == "active" && q.Allowed && q.Protocol == PreviewSharingIngressProtocol && q.GrantID == m.GrantID && q.DeploymentID == m.DeploymentID &&
		q.Generation == m.Generation && q.Destination == m.Destination && q.ExpiresAt == m.ExpiresAt && time.Now().Unix() < q.ExpiresAt
}

func sharingDeny(w http.ResponseWriter) error {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("WWW-Authenticate", `Basic realm="Shared preview", charset="UTF-8"`)
	http.Error(w, "Shared preview access was refused.", http.StatusUnauthorized)
	return nil // Never attach visitor/credential/network diagnostics to Caddy logs.
}

func (m *PreviewSharingMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if !m.localPolicy() || len(r.Header.Values("Authorization")) != 1 || r.Host != m.Destination || time.Now().Unix() >= m.ExpiresAt || (r.Method != http.MethodGet && r.Method != http.MethodHead) ||
		r.URL.User != nil || r.Header.Get("Upgrade") != "" || r.URL.RawQuery != "" {
		return sharingDeny(w)
	}
	username, credential, ok := r.BasicAuth()
	if !ok || username != "share" || !sharingCredential.MatchString(credential) {
		return sharingDeny(w)
	}
	ctx, cancel := context.WithDeadline(r.Context(), time.Unix(m.ExpiresAt, 0))
	defer cancel()
	if !m.authorize(ctx, credential) {
		return sharingDeny(w)
	}
	forwarded := r.Clone(ctx)
	// Read-only first increment: only content negotiation crosses into the
	// existing preview app. No session, share password or visitor IP forwarded.
	forwarded.Header = make(http.Header)
	for _, name := range []string{"Accept", "Accept-Encoding", "Accept-Language", "Range"} {
		if value := r.Header.Get(name); value != "" {
			forwarded.Header.Set(name, value)
		}
	}
	forwarded.RemoteAddr = ""
	// Responses cannot relax the sharing boundary's cache/referrer policy.
	guarded := &sharingResponse{ResponseWriter: w}
	if err := next.ServeHTTP(guarded, forwarded); err != nil && !guarded.wrote {
		http.Error(guarded, "Shared preview upstream is unavailable.", http.StatusBadGateway)
	}
	return nil
}

type sharingResponse struct {
	http.ResponseWriter
	wrote bool
}

func (w *sharingResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *sharingResponse) WriteHeader(status int) {
	if w.wrote {
		return
	}
	// Informational responses (103 Early Hints and the like) are dropped: they
	// would carry app-controlled preload hints to the visitor before the final
	// policy applies, and must not mark the headers as written.
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		return
	}
	w.wrote = true
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Del("Set-Cookie")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.ResponseWriter.WriteHeader(status)
}
func (w *sharingResponse) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}
func (w *sharingResponse) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

var _ caddy.Provisioner = (*PreviewSharingMiddleware)(nil)
var _ caddy.Validator = (*PreviewSharingMiddleware)(nil)
var _ caddyhttp.MiddlewareHandler = (*PreviewSharingMiddleware)(nil)

// Local tombstone journal is mandatory. It is re-read on EACH request;
// reload/restart and a lost API cannot resurrect an old route config.
func (m *PreviewSharingMiddleware) localPolicy() bool {
	root, err := os.OpenRoot(filepath.Dir(m.Journal))
	if err != nil {
		return false
	}
	defer root.Close()
	f, err := root.Open(filepath.Base(m.Journal))
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || (runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0) || st.Size() > 4096 {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(raw) > 4096 {
		return false
	}
	var q struct {
		sharingPolicy
		State string `json:"state"`
	}
	if json.Unmarshal(raw, &q) != nil {
		return false
	}
	return q.State == "active" && q.Protocol == PreviewSharingIngressProtocol && q.GrantID == m.GrantID && q.DeploymentID == m.DeploymentID &&
		q.Generation == m.Generation && q.Destination == m.Destination && q.ExpiresAt == m.ExpiresAt && time.Now().Unix() < q.ExpiresAt
}

func (m *PreviewSharingMiddleware) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		if d.NextArg() {
			return d.ArgErr()
		}
		for d.NextBlock(0) {
			key := d.Val()
			var value string
			if !d.AllArgs(&value) {
				return d.ArgErr()
			}
			switch key {
			case "grant_id":
				m.GrantID = value
			case "deployment_id":
				m.DeploymentID = value
			case "destination":
				m.Destination = value
			case "journal":
				m.Journal = value
			case "api_url":
				m.APIURL = value
			case "authorized_origin":
				m.AuthorizedOrigin = value
			case "agent_id":
				m.AgentID = value
			case "agent_secret":
				m.AgentSecret = value
			case "generation":
				n, e := strconv.Atoi(value)
				if e != nil {
					return d.Err("invalid generation")
				}
				m.Generation = n
			case "expires_at":
				n, e := strconv.ParseInt(value, 10, 64)
				if e != nil {
					return d.Err("invalid deadline")
				}
				m.ExpiresAt = n
			default:
				return d.Err("unknown preview sharing option")
			}
		}
	}
	return nil
}
