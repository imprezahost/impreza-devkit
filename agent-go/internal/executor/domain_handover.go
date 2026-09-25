package executor

import (
	"context"
	"crypto/tls"
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
		return rollback("The new HTTPS endpoint could not be verified.")
	}
	if err := writeAtomic(envPath, []byte(renderEnv(p.Vars)), 0600); err != nil {
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
	if err := d.Proxy.FinishDomainHandover(p.DeploymentID, h.After); err != nil {
		return failed("recovery_required", "Domain handover recovery record could not be finalized.")
	}
	return result
}

// Pin the connection to the inspected host proxy; validate SNI and its public
// certificate, never follow redirects or use proxy environment variables.
func (d *Docker) probeHandoverTLS(ctx context.Context, hostname string) error {
	probe, cancel := context.WithTimeout(ctx, 90*time.Second)
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
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: hostname},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), "443"))
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
		case <-time.After(2 * time.Second):
		}
	}
}

// Keep hostnames canonical before using them as an SNI or a route header.
var domainHandoverHost = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

func validHandoverHost(s string) bool {
	return len(s) <= 253 && domainHandoverHost.MatchString(s) && !strings.HasSuffix(s, ".onion")
}
