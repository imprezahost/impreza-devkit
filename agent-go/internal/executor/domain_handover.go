package executor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/proxy"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func (d *Docker) domainHandover(ctx context.Context, cmd *sdkclient.PollCommand, p sdkclient.UpdateRoutesPayload) sdkclient.DeployResult {
	h := p.DomainHandover
	if h == nil || h.Protocol != sdkclient.DomainHandoverProtocol || !recoveryDeploymentID.MatchString(p.DeploymentID) ||
		!handoverCommand.MatchString(cmd.ID) || cmd.ControlToken == "" || cmd.ProgressProtocol != sdkclient.DeploymentProgressProtocol || p.ProvisionOnion ||
		(h.Before != "" && !validHandoverHost(h.Before)) || !validHandoverHost(h.After) || h.Before == h.After || len(p.Routes) != 1 || p.Routes[0].Hostname != h.After || d.Proxy == nil {
		return failResult(cmd.ID, "Domain handover identity, journal protocol or proxy is unavailable.")
	}
	u, err := url.Parse(envValue(p.Vars, "DOMAIN_URL"))
	if err != nil || u.Scheme != "https" || u.Host != h.After || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return failResult(cmd.ID, "Domain handover URL cannot be verified.")
	}
	failed := func(status, message string) sdkclient.DeployResult {
		r := failResult(cmd.ID, message)
		r.DeploymentID = p.DeploymentID
		r.DomainHandover = &sdkclient.DomainHandoverResult{Before: h.Before, After: h.After, Status: status}
		return r
	}
	if err := d.deploymentCheckpoint(ctx, cmd, "preparing"); err != nil {
		return failed("unchanged", "Domain handover preparation was not authorized.")
	}
	envPath := filepath.Join(d.appDir(p.DeploymentID), ".env")
	info, err := os.Lstat(envPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return failed("unchanged", "Application environment cannot be verified.")
	}
	original, err := os.ReadFile(envPath)
	if err != nil {
		return failed("unchanged", "Application environment is unavailable.")
	}
	p.Vars, err = d.preserveServiceBindingVars(ctx, p.DeploymentID, p.Vars)
	if err != nil {
		return failed("unchanged", "Application connection ownership cannot be verified.")
	}
	route := p.Routes[0]
	if route.TLS != nil && route.TLS.Mode != "" && route.TLS.Mode != "letsencrypt" {
		return failed("unchanged", "Domain handover requires verified public HTTPS.")
	}
	upstream := route.Upstream
	if upstream == "" {
		return failed("unchanged", "Domain handover requires an explicit application upstream.")
	}
	target := proxy.Route{Hostname: h.After, Upstream: upstream, TLSMode: "letsencrypt", BasicAuth: basicAuthFromPayload(route.BasicAuth)}
	if route.TLS != nil {
		target.TLSEmail = route.TLS.Email
		target.TLSDNSProvider = route.TLS.DNSProvider
	}
	if route.Onion != nil && route.Onion.Enabled {
		target.OnionAddr = p.OnionAddr
	}
	if err := d.deploymentCheckpoint(ctx, cmd, "replacing"); err != nil {
		return failed("unchanged", "Domain handover replacement was not authorized.")
	}
	recovery := &domainHandoverRecord{Version: 1, Command: cmd.ID, Identity: DomainHandoverIdentity{DeploymentID: p.DeploymentID, Before: h.Before, After: h.After}, Environment: original}
	journalPath, journalErr := d.handoverPath(cmd.ID)
	if journalErr != nil {
		return failed("unchanged", "Domain recovery storage is unavailable.")
	}
	if _, err := os.Lstat(journalPath); !os.IsNotExist(err) {
		return failed("recovery_required", "A previous domain recovery record requires reconciliation.")
	}
	if err := d.saveDomainHandover(recovery); err != nil {
		return failed("unchanged", "Domain recovery storage could not be saved.")
	}
	undo, err := d.Proxy.BeginDomainHandover(ctx, p.DeploymentID, h.Before, h.After, []proxy.Route{target})
	if err != nil {
		return failed("recovery_required", "Domain handover could not replace the proxy route.")
	}
	rollback := func(message string) sdkclient.DeployResult {
		envErr := writeAtomic(envPath, original, 0600)
		if envErr == nil {
			envErr = syncRecoveryDirectory(filepath.Dir(envPath))
		}
		routeErr := undo(context.Background())
		if envErr != nil || routeErr != nil {
			return failed("recovery_required", message+" Previous configuration requires reconciliation.")
		}
		result := failed("rolled_back", message+" Previous configuration restored.")
		recovery.Result = &result
		if err := d.saveDomainHandover(recovery); err != nil {
			return failed("recovery_required", "Domain rollback receipt could not be saved.")
		}
		return result
	}
	if err := d.probeHandoverTLS(ctx, h.After); err != nil {
		// Say WHY in terms a customer reads — the certificate
		// issuance, not "verification", is what usually needs the time
		// (the ACME dns-01 challenge can need a retry while the new TXT
		// record propagates). No hostname or address is added here.
		return rollback("The new domain's certificate did not become ready in time (the certificate authority can need a retry while the new DNS record propagates); the previous domain keeps serving.")
	}
	env, err := renderEnv(p.Vars)
	if err != nil {
		return rollback("The application environment could not be rendered.")
	}
	if err := writeAtomic(envPath, []byte(env), 0600); err != nil {
		return rollback("The application environment could not be saved.")
	}
	if err := syncRecoveryDirectory(filepath.Dir(envPath)); err != nil {
		return rollback("Domain configuration durability could not be confirmed.")
	}
	result := sdkclient.DeployResult{CommandID: cmd.ID, DeploymentID: p.DeploymentID, Status: "success", Domain: u.String(), Onion: p.OnionAddr,
		DomainHandover: &sdkclient.DomainHandoverResult{Before: h.Before, After: h.After, Status: "switched"}}
	recovery.Result = &result
	if err := d.saveDomainHandover(recovery); err != nil {
		return rollback("Domain completion receipt could not be saved.")
	}
	// only now does the previous domain stop serving (the overlap is replaced by the new route alone).
	if err := d.Proxy.FinishDomainHandover(ctx, p.DeploymentID, h.Before, h.After); err != nil {
		return failed("recovery_required", "Domain handover recovery record could not be finalized.")
	}
	return result
}

// The verification window must cover a full ACME retry cycle. A real
// handover showed the failure: the first dns-01 challenge answer
// came back NXDOMAIN while the new TXT record propagated, Caddy schedules
// its next attempt handoverACMERetryDelay later, and the issuance after a
// good answer takes tens of seconds — the old flat 90 s ceiling expired
// mid-flight and rolled back a handover that was about to succeed. The
// budget is one retry + issuance + slack; a certificate that does not
// come out within it still rolls the handover back, exactly as before.
const (
	handoverACMERetryDelay = 60 * time.Second
	handoverACMEIssuance   = 30 * time.Second
	handoverTLSSlack       = 90 * time.Second
	handoverTLSBudget      = handoverACMERetryDelay + handoverACMEIssuance + handoverTLSSlack
)

// Test seams: shrunk by the unit tests so a full retry cycle fits in
// milliseconds; production uses the constants above.
var (
	handoverTLSBudgetNow = handoverTLSBudget
	handoverTLSRetry     = 2 * time.Second
)

// Pin the connection to the inspected host proxy; validate SNI and its public
// certificate, never follow redirects or use proxy environment variables.
func (d *Docker) probeHandoverTLS(ctx context.Context, hostname string) error {
	probe, cancel := context.WithTimeout(ctx, handoverTLSBudgetNow)
	defer cancel()
	raw, err := limitedRuntimeOutput(d.dockerCmd(probe, "inspect", proxy.ContainerName), 1024*1024)
	var containers []struct {
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	if err != nil || json.Unmarshal(raw, &containers) != nil || len(containers) != 1 {
		return errors.New("proxy identity unavailable")
	}
	ip := net.ParseIP(containers[0].NetworkSettings.Networks[proxy.NetworkName].IPAddress)
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return errors.New("proxy network identity unavailable")
	}
	return awaitVerifiedHTTPS(probe, net.JoinHostPort(ip.String(), "443"), hostname, nil)
}

// awaitVerifiedHTTPS polls https://<hostname>/ pinned to dialAddr (the
// proxy's address) until the handshake validates SNI against rootCAs
// (nil = the system roots, the production path) and the app answers a
// normal status. The caller owns the deadline: reaching it is the
// failure, an HTTP 421 or a 5xx just means the route or the certificate
// is not serving yet.
func awaitVerifiedHTTPS(probe context.Context, dialAddr, hostname string, rootCAs *x509.CertPool) error {
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: hostname, RootCAs: rootCAs},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", dialAddr)
		}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for {
		req, err := http.NewRequestWithContext(probe, http.MethodGet, "https://"+hostname+"/", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(req)
		if response != nil {
			response.Body.Close()
			if err == nil && response.StatusCode >= 200 && response.StatusCode < 500 && response.StatusCode != 421 {
				return nil
			}
		}
		select {
		case <-probe.Done():
			return errors.New("verified HTTPS route did not become ready")
		case <-time.After(handoverTLSRetry):
		}
	}
}

// Keep hostnames canonical before using them as an SNI or a route header.
var domainHandoverHost = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

func validHandoverHost(s string) bool {
	return len(s) <= 253 && domainHandoverHost.MatchString(s) && !strings.HasSuffix(s, ".onion")
}
