package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Ready-swap redeploys (ready-swap-v1) move a deployment's routes between two
// containers of the same deployment, `<id>-app` and `<id>-app-next`. Only the
// upstream host of the deployment's own fragment changes: hostnames, TLS,
// Shield and basic auth stay byte for byte, and no other deployment's
// fragment is read or written. The reload is Caddy's graceful one; a
// rejected configuration restores the previous fragment and reloads again.

var retargetHostPattern = regexp.MustCompile(`^dpl_[A-Za-z0-9_-]{1,40}-app(-next)?$`)

// ErrRetargetShape means the fragment does not have the shape a ready swap
// can rewrite exactly (no upstream, an upstream of another container, or a
// port that differs between routes).
var ErrRetargetShape = errors.New("routing fragment is not a single-container deployment")

// UpstreamHost reports which container a deployment's fragment points at
// (every route of the fragment must agree) and the upstream port.
func (c *Caddy) UpstreamHost(deploymentID string) (host string, port string, err error) {
	raw, err := os.ReadFile(filepath.Join(c.StateDir, "deployments", deploymentID+".caddy"))
	if err != nil {
		return "", "", err
	}
	return fragmentUpstream(string(raw), deploymentID)
}

func fragmentUpstream(raw, deploymentID string) (string, string, error) {
	host, port := "", ""
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "  reverse_proxy ") {
			continue
		}
		target := strings.TrimPrefix(line, "  reverse_proxy ")
		h, p, ok := strings.Cut(target, ":")
		if !ok || !retargetHostPattern.MatchString(h) || !strings.HasPrefix(h, deploymentID+"-app") || strings.TrimSpace(p) != p || p == "" {
			return "", "", ErrRetargetShape
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return "", "", ErrRetargetShape
			}
		}
		if (host != "" && h != host) || (port != "" && p != port) {
			return "", "", ErrRetargetShape
		}
		host, port = h, p
	}
	if host == "" {
		return "", "", ErrRetargetShape
	}
	return host, port, nil
}

// LiveUpstream reports which container the RUNNING proxy sends the
// deployment's routes to, read from the configuration Caddy has loaded (its
// loopback admin API, inside the proxy container). The fragment on disk can
// be ahead of it: an agent stopped between writing the fragment and the
// reload leaves the file on one container and the proxy on the other. Every
// reverse_proxy dial of this deployment must agree, or it is an error.
func (c *Caddy) LiveUpstream(ctx context.Context, deploymentID string) (string, error) {
	if !retargetHostPattern.MatchString(deploymentID + "-app") {
		return "", fmt.Errorf("invalid deployment identity")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var raw []byte
	var err error
	if c.liveConfig != nil {
		raw, err = c.liveConfig(ctx)
	} else {
		raw, err = exec.CommandContext(ctx, "docker", "exec", ContainerName,
			"wget", "-qO-", "http://127.0.0.1:2019/config/apps/http").Output()
	}
	if err != nil {
		return "", fmt.Errorf("the running proxy configuration could not be read: %w", err)
	}
	if len(raw) > 32<<20 {
		return "", errors.New("the running proxy configuration is too large")
	}
	var config any
	if err := json.Unmarshal(raw, &config); err != nil {
		return "", errors.New("the running proxy configuration is not JSON")
	}
	host := ""
	var walk func(v any) error
	walk = func(v any) error {
		switch t := v.(type) {
		case map[string]any:
			if dial, ok := t["dial"].(string); ok {
				h, _, found := strings.Cut(dial, ":")
				if found && (h == deploymentID+"-app" || h == deploymentID+"-app-next") {
					if host != "" && host != h {
						return ErrRetargetShape
					}
					host = h
				}
			}
			for _, child := range t {
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range t {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(config); err != nil {
		return "", err
	}
	if host == "" {
		return "", errors.New("the running proxy has no route to this deployment")
	}
	return host, nil
}

// RetargetUpstream points every route of the deployment at container `to`
// (same port). It is idempotent: a fragment already on `to` only reloads.
func (c *Caddy) RetargetUpstream(ctx context.Context, deploymentID, to string) error {
	if !retargetHostPattern.MatchString(to) || !strings.HasPrefix(to, deploymentID+"-app") {
		return fmt.Errorf("invalid upstream container")
	}
	if err := c.guardRoutingSwitch(); err != nil {
		return err
	}
	path := filepath.Join(c.StateDir, "deployments", deploymentID+".caddy")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("routing fragment unreadable: %w", err)
	}
	from, port, err := fragmentUpstream(string(raw), deploymentID)
	if err != nil {
		return err
	}
	if from != to {
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			if strings.HasPrefix(line, "  reverse_proxy ") {
				lines[i] = "  reverse_proxy " + to + ":" + port
			}
		}
		if err := writeSwitchFile(path, []byte(strings.Join(lines, "\n"))); err != nil {
			return fmt.Errorf("write routing fragment: %w", err)
		}
	}
	restore := func(cause error) error {
		recovery, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := writeSwitchFile(path, raw); err != nil {
			return errors.Join(cause, err)
		}
		if err := c.regenerateCaddyfile(); err != nil {
			return errors.Join(cause, err)
		}
		return errors.Join(cause, c.reloadSwitch(recovery))
	}
	if err := c.regenerateCaddyfile(); err != nil {
		return restore(err)
	}
	if err := c.reloadSwitch(ctx); err != nil {
		return restore(err)
	}
	return nil
}
