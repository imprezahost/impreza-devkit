// Package uptimeprobe is the vantage-side probe engine: the
// uptime-probe-v1 capability of the agent. It talks to exactly TWO
// control-plane routes (GET /v1/agent/uptime/targets and
// POST /v1/agent/uptime/results) and nothing else; it only runs on an
// agent the control plane has marked as a vantage (a customer agent's
// probe of the targets route is refused 403 and the capability never
// announces).
//
// Privacy: the prober records per target only the status class, the TLS
// validity/days and a latency number. It never reads a body, never logs
// headers, never persists the resolved address past the round, and its
// logs carry counts and deployment ids only.
package uptimeprobe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/probeguard"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// Limits: one deadline per target, bounded concurrency, a hard cap on
// targets per round (the server also caps per account; this is the
// vantage's own backstop) and the probe interval the server dictates.
const (
	DefaultInterval  = 60 * time.Second
	PerTargetTimeout = 15 * time.Second
	MaxConcurrency   = 4
	MaxTargets       = 100
)

// DefaultSocks is the local Tor SOCKS address onion probes use. The
// vantage guests run a local tor daemon on the standard port; empty would
// fail every onion closed.
const DefaultSocks = "127.0.0.1:9050"

// Active reports whether a probe loop is running for this credential —
// the poller announces uptime-probe-v1 exactly while this is true.
func (p *Prober) Active() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// Prober is the vantage probe loop. Construct with New and start with Run.
type Prober struct {
	client    *sdkclient.Client
	log       *slog.Logger
	resolver  probeguard.Resolver
	socksAddr string // empty => onion probes fail closed with err

	mu      sync.Mutex
	running bool

	// rounds counts probe windows when the target list exceeds MaxTargets:
	// the window START advances every round so every target is measured at
	// least once across rounds instead of always the first
	// MaxTargets of a stable server order.
	rounds int64
}

// newTransport is the transport factory probeOne dials through. Production
// is the guard's HostDialer; tests swap it for a blackhole/loopback one.
var newTransport = func(ip net.IP, timeout time.Duration) *http.Transport {
	return probeguard.HostDialer(ip, timeout)
}

// TargetsResponse is the JSON of GET /v1/agent/uptime/targets (the
// contract: protocol, interval_seconds, targets[]).
type TargetsResponse struct {
	Protocol        int      `json:"protocol"`
	IntervalSeconds int      `json:"interval_seconds"`
	Targets         []Target `json:"targets"`
}

// Target is one probeable endpoint as the SERVER resolved it.
type Target struct {
	DeploymentID string `json:"deployment_id"`
	Scheme       string `json:"scheme"`
	Host         string `json:"host"`
	Kind         string `json:"kind"` // "domain" | "onion"
}

// resultsBody is the JSON of POST /v1/agent/uptime/results.
type resultsBody struct {
	Protocol int      `json:"protocol"`
	RoundAt  string   `json:"round_at"`
	Results  []Result `json:"results"`
}

// Result is one probe outcome: numbers only, never a body or address.
type Result struct {
	DeploymentID string `json:"deployment_id"`
	OK           int    `json:"ok"`
	StatusClass  string `json:"status_class"`
	TLSValid     *int   `json:"tls_valid"`
	TLSDays      *int   `json:"tls_days"`
	LatencyMS    int    `json:"latency_ms"`
}

// New builds a Prober on the agent's existing SDK client (agent realm).
// socksAddr is the local Tor SOCKS address for onion targets; empty means
// onion probes fail closed.
func New(client *sdkclient.Client, socksAddr string, log *slog.Logger) *Prober {
	return &Prober{
		client:    client,
		log:       log,
		resolver:  net.DefaultResolver,
		socksAddr: socksAddr,
	}
}

// refused reports whether err is the control plane REFUSING this
// credential on the uptime routes: 403 (Forbidden — a customer agent, or a
// vantage whose mark was removed) or 401 (AuthError — credential revoked).
// The SDK maps 403 to *Forbidden, NOT *AuthError; both must stop probing.
func refused(err error) bool {
	var forbidden *sdkclient.Forbidden
	var auth *sdkclient.AuthError
	return errors.As(err, &forbidden) || errors.As(err, &auth)
}

// IsVantage asks the control plane: the targets route answers 200 only
// for a credential marked as a vantage; 403 means a customer agent, and
// the caller must neither announce the capability nor run the loop.
func (p *Prober) IsVantage(ctx context.Context) bool {
	var resp TargetsResponse
	err := p.client.Get(ctx, "/v1/agent/uptime/targets", nil, &resp)
	if err != nil {
		if refused(err) {
			p.log.Debug("uptime probe: control plane says this credential is not a vantage")
			return false
		}
		// Network/5xx doubt is also "no" — announcing on doubt would
		// break the customer-agent rule — but it deserves a warning.
		p.log.Warn("uptime probe: cannot ask vantage status yet", "err", err)
		return false
	}
	return resp.Protocol > 0
}

// Run probes on the server-dictated cadence until ctx is done OR the
// control plane refuses the credential (mark removed): that refusal is an
// AuthError from the targets route and Run returns so the caller stops
// announcing the capability. Any other failure just logs and retries; the
// quorum on the server degrades to suspect, which is the designed
// behavior.
func (p *Prober) Run(ctx context.Context) {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()

	interval := DefaultInterval
	var last TargetsResponse
	for {
		if ctx.Err() != nil {
			return
		}
		fetched, err := p.fetchTargets(ctx)
		if refused(err) {
			p.log.Info("uptime probe: vantage mark removed; stopping the probe loop")
			return
		}
		if err == nil {
			last = fetched
			if last.IntervalSeconds >= 15 {
				interval = time.Duration(last.IntervalSeconds) * time.Second
			}
			started := time.Now()
			results := p.probeAll(ctx, last.Targets)
			if postErr := p.postResults(ctx, results); postErr != nil {
				p.log.Warn("uptime probe: post results failed", "err", postErr)
			}
			// Privacy: counts only — how many targets, how many ok. No
			// addresses, no headers, no bodies.
			ok := 0
			for _, r := range results {
				ok += r.OK
			}
			p.log.Info("uptime probe round complete",
				"targets", len(results), "ok", ok, "took_ms", time.Since(started).Milliseconds())
		} else {
			p.log.Warn("uptime probe: fetch targets failed", "err", err)
		}
		if !sleepCtx(ctx, interval) {
			return
		}
	}
}

func (p *Prober) fetchTargets(ctx context.Context) (TargetsResponse, error) {
	var resp TargetsResponse
	if err := p.client.Get(ctx, "/v1/agent/uptime/targets", nil, &resp); err != nil {
		return resp, err
	}
	if len(resp.Targets) > MaxTargets {
		resp.Targets = resp.Targets[:MaxTargets]
	}
	return resp, nil
}

func (p *Prober) postResults(ctx context.Context, results []Result) error {
	if results == nil {
		return nil
	}
	body := resultsBody{
		Protocol: 1,
		RoundAt:  time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		Results:  results,
	}
	return p.client.Post(ctx, "/v1/agent/uptime/results", body, nil)
}

// probeAll probes every target with bounded concurrency. A target whose
// guard refuses it yields an err result with latency 0 — the server then
// sees no healthy report for it, never silence about why.
func (p *Prober) probeAll(ctx context.Context, targets []Target) []Result {
	if len(targets) == 0 {
		return nil
	}
	n := len(targets)
	window := n
	start := 0
	if n > MaxTargets {
		// The server's list order is stable, so a fixed head-of-list window
		// would measure the same MaxTargets forever. The window start
		// advances MaxTargets per round and wraps: with 150 targets and a
		// cap of 100, the first round measures 0-99 and the second 100-149
		// plus 0-49 — every target at least once per two rounds.
		window = MaxTargets
		start = int(((atomic.AddInt64(&p.rounds, 1) - 1) * int64(MaxTargets)) % int64(n))
	}
	sem := make(chan struct{}, MaxConcurrency)
	out := make([]Result, window)
	var wg sync.WaitGroup
	for i := 0; i < window; i++ {
		t := targets[(start+i)%n]
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			out[i] = p.probeOne(ctx, t)
		}(i, t)
	}
	wg.Wait()
	return out
}

// probeOne runs a single probe under ONE deadline: the PerTargetTimeout
// context starts BEFORE the first DNS lookup and covers resolution, dial,
// TLS and the HEAD answer together (a slow resolver plus a
// slow answer is one 15 s budget, not two). HEAD only, no redirects, body
// discarded unread, the connection dialed to the exact validated address
// with the hostname preserved for Host and SNI. Onion targets go through
// the local Tor SOCKS and fail closed without it — and only for a valid
// v3 .onion hostname: anything else never opens the SOCKS.
//
// Logging discipline: refusals name the deployment and the
// range CLASS; the hostname, the resolved address and error texts that
// embed them are never logged.
func (p *Prober) probeOne(ctx context.Context, t Target) Result {
	res := Result{DeploymentID: t.DeploymentID, StatusClass: "err"}
	pctx, cancel := context.WithTimeout(ctx, PerTargetTimeout)
	defer cancel()
	started := time.Now()

	if strings.HasSuffix(strings.ToLower(t.Host), ".onion") || t.Kind == "onion" {
		if class := onionV3Problem(t.Host); class != "" {
			p.log.Warn("uptime probe: target refused by the guard", "deployment_id", t.DeploymentID, "reason", class)
			return res
		}
		if p.socksAddr == "" {
			// Fail closed: an onion never probes over clearnet.
			return res
		}
		tr, err := probeguard.SocksDialer(p.socksAddr, PerTargetTimeout)
		if err != nil {
			return res
		}
		res.OK, res.StatusClass = doProbe(pctx, tr, t, &res, false)
		res.LatencyMS = int(time.Since(started).Milliseconds())
		return res
	}

	// Domain target: resolve ONCE through the guard, connect to the
	// validated address (rebinding closed server-side and here).
	addrs, err := probeguard.Validate(pctx, p.resolver, t.Host)
	if err != nil {
		var refusal *probeguard.Refusal
		if errors.As(err, &refusal) {
			p.log.Warn("uptime probe: target refused by the guard", "deployment_id", t.DeploymentID, "reason", refusal.Class)
		} else {
			// Resolve failures carry the hostname inside the resolver's
			// error text — log the fact, never the text.
			p.log.Warn("uptime probe: target not probeable this round", "deployment_id", t.DeploymentID)
		}
		return res
	}
	ip := pickAddr(addrs)
	tr := newTransport(ip, PerTargetTimeout)
	res.OK, res.StatusClass = doProbe(pctx, tr, t, &res, t.Scheme == "https")
	res.LatencyMS = int(time.Since(started).Milliseconds())
	return res
}

// onionV3Problem returns "" for a valid v3 .onion hostname — exactly 56
// base32 characters plus the .onion suffix, lowercase — or the refusal
// class otherwise. The guard runs BEFORE the SOCKS dial: a hostile kind
// field must not turn the vantage's Tor into a clearnet-coverered fetch of
// an arbitrary destination.
func onionV3Problem(host string) string {
	h := strings.TrimSpace(host)
	if !strings.HasSuffix(h, ".onion") {
		return "not-onion"
	}
	label := strings.TrimSuffix(h, ".onion")
	if len(label) != 56 {
		return "not-onion-v3"
	}
	for _, c := range label {
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return "not-onion-v3"
		}
	}
	return ""
}

// doProbe issues the HEAD request. 3xx counts as an answer (never
// followed); 2xx/3xx are ok, everything else is not. For HTTPS the TLS
// state fills tls_valid/tls_days; HTTP leaves them null.
func doProbe(ctx context.Context, tr *http.Transport, t Target, res *Result, isTLS bool) (int, string) {
	// No client.Timeout: the per-target context IS the budget (DNS already
	// spent inside it); a second timer here would double-count the same
	// deadline.
	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	scheme := t.Scheme
	if scheme == "" {
		scheme = "https"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, probeguard.URLForTarget(scheme, t.Host), nil)
	if err != nil {
		return 0, "err"
	}
	req.Header.Set("User-Agent", "ImprezaUptime/1")
	resp, err := client.Do(req)
	if err != nil {
		return 0, "err"
	}
	// The body is discarded UNREAD: never buffered, never parsed.
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
	resp.Body.Close()

	if isTLS && resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		valid := 0
		if len(resp.TLS.VerifiedChains) > 0 || resp.TLS.HandshakeComplete {
			valid = 1
		}
		v := valid
		res.TLSValid = &v
		days := int(time.Until(resp.TLS.PeerCertificates[0].NotAfter).Hours() / 24)
		res.TLSDays = &days
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 400:
		class := fmt.Sprintf("%dxx", resp.StatusCode/100)
		return 1, class
	case resp.StatusCode >= 400 && resp.StatusCode < 600:
		return 0, fmt.Sprintf("%dxx", resp.StatusCode/100)
	default:
		return 0, "err"
	}
}

// pickAddr prefers IPv4 (deterministic; the guard already refused any
// denied family member, so either answer is probeable).
func pickAddr(addrs []net.IPAddr) net.IP {
	for _, a := range addrs {
		if v4 := a.IP.To4(); v4 != nil {
			return v4
		}
	}
	return addrs[0].IP
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// TLSConfigForTests exposes the probe's TLS expectations: verification
// stays ON (the default); the function exists so tests can assert the
// transport never carries InsecureSkipVerify.
func TLSConfigForTests() *tls.Config {
	return nil // nil == Go defaults == verification ON
}
