package executor

// Onion repository requests use a dedicated short-lived Tor client with
// remote hostname resolution. It is separate from the hidden-services
// daemon, binds only to loopback, and has no direct connection fallback.

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/proxy"
)

// torDaemonImage is the same digest-pinned Tor image the shared daemon
// uses — one supply chain, no floating tags.
var torDaemonImage = proxy.TorImage

// onionGitHost reports whether a git URL points at a Tor v3 onion service.
func onionGitHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return regexp.MustCompile(`^[a-z2-7]{56}\.onion$`).MatchString(strings.ToLower(host))
}

// onionProxyCmd is the command runner; tests replace it. Output semantics
// mirror exec.CommandContext(...).CombinedOutput().
var onionProxyCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// onionCloneProxy is a disposable Tor SOCKS client for one clone.
type onionCloneProxy struct {
	container string
	port      string
}

// startOnionCloneProxy launches the ephemeral client when the URL needs it
// (nil proxy, nil error otherwise). Readiness is proven by resolving a name
// THROUGH Tor (tor-resolve), not by the TCP listener alone.
func startOnionCloneProxy(ctx context.Context, gitURL string) (*onionCloneProxy, error) {
	// Preserve ordinary scp-like SSH URLs. Onion SSH is deliberately unsupported.
	if !strings.Contains(gitURL, "://") && strings.Contains(gitURL, "@") {
		_, hostPath, _ := strings.Cut(gitURL, "@")
		host, _, _ := strings.Cut(hostPath, ":")
		if strings.HasSuffix(strings.TrimSuffix(strings.ToLower(host), "."), ".onion") {
			return nil, fmt.Errorf("onion SSH git is unsupported; use HTTP(S) over Tor")
		}
		return nil, nil
	}
	u, parseErr := url.Parse(gitURL)
	if parseErr != nil {
		return nil, fmt.Errorf("invalid git URL")
	}
	if !onionGitHost(gitURL) {
		if strings.HasSuffix(strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), ".onion") {
			return nil, fmt.Errorf("invalid v3 onion git host")
		}
		return nil, nil
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return nil, fmt.Errorf("onion git requires HTTP(S) without URL credentials")
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("impreza_gittor_%x", suffix)
	runCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	if out, err := onionProxyCmd(runCtx, "docker", "run", "-d", "--rm",
		"--name", name,
		"-p", "127.0.0.1::9050",
		"--memory", "128m",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		torDaemonImage,
		"tor", "--SocksPort", "0.0.0.0:9050", "--SafeLogging", "1", "--Log", "notice stdout"); err != nil {
		(&onionCloneProxy{container: name}).stop()
		return nil, fmt.Errorf("start tor client for onion clone: %w\n%s", err, tail(out, 512))
	}
	stop := func() {
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer stopCancel()
		_, _ = onionProxyCmd(stopCtx, "docker", "rm", "-f", name)
	}
	portOut, err := onionProxyCmd(runCtx, "docker", "port", name, "9050")
	if err != nil {
		stop()
		return nil, fmt.Errorf("discover tor client port: %w", err)
	}
	// `docker port` prints lines like 127.0.0.1:49153 — take the last one.
	lastLine := ""
	for _, line := range strings.Split(strings.TrimSpace(string(portOut)), "\n") {
		if strings.TrimSpace(line) != "" {
			lastLine = strings.TrimSpace(line)
		}
	}
	host, port, err := net.SplitHostPort(lastLine)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || host != "127.0.0.1" || portErr != nil || portNumber < 1 || portNumber > 65535 {
		stop()
		return nil, fmt.Errorf("parse tor client port %q: %w", lastLine, err)
	}
	// Readiness through Tor itself: tor-resolve inside the container proves
	// the client has bootstrapped before git gets a half-open SOCKS port.
	// A fresh Tor client can legitimately take over a minute to bootstrap
	// under consensus load; bounded, but generous.
	deadline := time.Now().Add(120 * time.Second)
	for {
		if err := runCtx.Err(); err != nil {
			stop()
			return nil, err
		}
		if out, err := onionProxyCmd(runCtx, "docker", "exec", name, "tor-resolve", "example.net", "127.0.0.1:9050"); err == nil && net.ParseIP(strings.TrimSpace(string(out))) != nil {
			break
		}
		if time.Now().After(deadline) {
			stop()
			return nil, fmt.Errorf("tor client for onion clone did not bootstrap in time")
		}
		select {
		case <-runCtx.Done():
			stop()
			return nil, runCtx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return &onionCloneProxy{container: name, port: port}, nil
}

// Cleanup is bounded and best-effort. A Docker failure can leave the named
// container behind; --rm alone does not remove a container that is still running.
func (p *onionCloneProxy) stop() {
	if p == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = onionProxyCmd(ctx, "docker", "rm", "-f", p.container)
}

// envEntries routes git through the client. socks5h hands the hostname to
// Tor unresolved — the agent's resolver never sees the onion name.
func (p *onionCloneProxy) envEntries() []string {
	proxy := "socks5h://127.0.0.1:" + p.port
	return []string{
		"http_proxy=" + proxy,
		"HTTP_PROXY=" + proxy,
		"NO_PROXY=",
		"no_proxy=",
		"https_proxy=" + proxy,
		"HTTPS_PROXY=" + proxy,
		"all_proxy=" + proxy,
		"ALL_PROXY=" + proxy,
	}
}
