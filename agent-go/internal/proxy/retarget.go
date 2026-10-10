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
// containers of the same deployment, `<id>-app` and `<id>-app-next`. On the
// standby-pair form the fragment carries BOTH upstreams with
// `lb_policy first` and an active health check: traffic flips by container
// lifecycle (the new copy is made and proven before the old one stops), the
// Caddyfile never changes and no reload runs. On the legacy single-upstream
// form only the upstream host of the deployment's own fragment changes
// (hostnames, TLS, Shield and basic auth stay byte for byte), and the reload
// is Caddy's graceful one; a rejected configuration restores the previous
// fragment and reloads again.

var retargetHostPattern = regexp.MustCompile(`^dpl_[A-Za-z0-9_-]{1,40}-app(-next)?$`)

// ErrRetargetShape means the fragment does not have the shape a ready swap
// can rewrite exactly (no upstream, an upstream of another container, or a
// port that differs between routes).
var ErrRetargetShape = errors.New("routing fragment is not a single-container deployment")

// ErrRetargetPair means the fragment is already the standby-pair form:
// a swap drives it by container lifecycle, never by rewriting the fragment.
var ErrRetargetPair = errors.New("routing fragment carries the standby pair; flip by container lifecycle, not by retarget")

// UpstreamHost reports which container a deployment's fragment PREFERS (the
// first slot of the pair form, the only slot of the legacy form) and the
// upstream port. The third return says whether the fragment is the
// standby-pair form.
func (c *Caddy) UpstreamHost(deploymentID string) (host string, port string, pair bool, err error) {
	raw, err := os.ReadFile(filepath.Join(c.StateDir, "deployments", deploymentID+".caddy"))
	if err != nil {
		return "", "", false, err
	}
	return fragmentUpstream(string(raw), deploymentID)
}

func fragmentUpstream(raw, deploymentID string) (string, string, bool, error) {
	host, port := "", ""
	pair := false
	inPairBlock := false
	for _, line := range strings.Split(raw, "\n") {
		if inPairBlock {
			// The standby pair block: subdirectives until the closing brace.
			if line == "  }" {
				inPairBlock = false
			}
			continue
		}
		if !strings.HasPrefix(line, "  reverse_proxy ") {
			continue
		}
		target := strings.TrimPrefix(line, "  reverse_proxy ")
		opensBlock := strings.HasSuffix(target, " {")
		if opensBlock {
			target = strings.TrimSuffix(target, " {")
		}
		fields := strings.Fields(target)
		if len(fields) != 1 && len(fields) != 2 {
			return "", "", false, ErrRetargetShape
		}
		h := fields[0]
		hostName, p, ok := strings.Cut(h, ":")
		if !ok || !retargetHostPattern.MatchString(hostName) || !strings.HasPrefix(hostName, deploymentID+"-app") || strings.TrimSpace(p) != p || p == "" {
			return "", "", false, ErrRetargetShape
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return "", "", false, ErrRetargetShape
			}
		}
		if len(fields) == 2 {
			if fields[1] != deploymentID+"-app-next:"+p || !opensBlock {
				return "", "", false, ErrRetargetShape
			}
			pair = true
			inPairBlock = true
		}
		if (host != "" && hostName != host) || (port != "" && p != port) {
			return "", "", false, ErrRetargetShape
		}
		host, port = hostName, p
	}
	if host == "" {
		return "", "", false, ErrRetargetShape
	}
	return host, port, pair, nil
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
					// The standby pair carries both dials; the PREFERRED slot
					// (the app one, first in the pool) is what "live"
					// means there. A same-slot disagreement is still an
					// error.
					if host != "" && host != h && host != deploymentID+"-app" && h != deploymentID+"-app" {
						return ErrRetargetShape
					}
					if host == "" || h == deploymentID+"-app" {
						host = h
					}
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

// SwapSpecOf reads the standby-pair spec back off a deployment's fragment
// (the pair block's own peer, health_uri and health_status). A route
// update rewrites the fragment from the payload's route list, which does
// not carry the swap policy — reading the pair back lets the rewrite keep
// it instead of silently downgrading the deployment to the reload form.
// Returns nil when the fragment is the legacy single-upstream form.
func (c *Caddy) SwapSpecOf(deploymentID string) (*SwapSpec, error) {
	raw, err := os.ReadFile(filepath.Join(c.StateDir, "deployments", deploymentID+".caddy"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "  reverse_proxy ") || !strings.HasSuffix(line, " {") {
			continue
		}
		fields := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(line, "  reverse_proxy "), " {"))
		if len(fields) != 2 || fields[1] != deploymentID+"-app-next:"+strings.SplitN(fields[0], ":", 2)[1] {
			continue
		}
		spec := &SwapSpec{Peer: fields[1]}
		for _, sub := range lines[i+1:] {
			if sub == "  }" {
				break
			}
			if v, ok := strings.CutPrefix(sub, "    health_uri "); ok {
				spec.Path = v
			}
			if v, ok := strings.CutPrefix(sub, "    health_status "); ok {
				spec.StatusClass = v
			}
		}
		if spec.Path == "" || spec.StatusClass == "" {
			return nil, ErrRetargetShape
		}
		return spec, nil
	}
	return nil, nil
}

// RetargetUpstream points every route of the deployment at container `to`
// (same port). It only applies to the LEGACY single-upstream form: the
// standby pair refuses (traffic flips by container lifecycle). It is
// idempotent on the single form: a fragment already on `to` only reloads.
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
	from, port, pair, err := fragmentUpstream(string(raw), deploymentID)
	if err != nil {
		return err
	}
	if pair {
		return ErrRetargetPair
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

// EnableSwapPeer upgrades a deployment's legacy single-upstream
// fragment to the standby-pair form: every `reverse_proxy <id>-app:port`
// line becomes the pair block with `lb_policy first` and the active health
// check, and Caddy reloads once — the one transitional reload. From then on
// ready swaps flip traffic by container lifecycle and the Caddyfile stays
// byte-identical. Idempotent: a fragment already in the pair form is left
// alone (no write, no reload).
func (c *Caddy) EnableSwapPeer(ctx context.Context, deploymentID string, spec SwapSpec) error {
	if err := c.guardRoutingSwitch(); err != nil {
		return err
	}
	if !swapPeerShapeRe.MatchString(spec.Peer) || !strings.HasPrefix(spec.Peer, deploymentID+"-app-next:") {
		return fmt.Errorf("invalid standby container")
	}
	if !swapPathRe.MatchString(spec.Path) || strings.Contains(spec.Path, "..") {
		return fmt.Errorf("swap readiness path is not safe")
	}
	if !swapClassRe.MatchString(spec.StatusClass) {
		return fmt.Errorf("swap status class must be 1xx-5xx")
	}
	path := filepath.Join(c.StateDir, "deployments", deploymentID+".caddy")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("routing fragment unreadable: %w", err)
	}
	_, _, pair, err := fragmentUpstream(string(raw), deploymentID)
	if err != nil {
		return err
	}
	if pair {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "  reverse_proxy ") {
			h, p, _, ferr := fragmentUpstream(line+"\n", deploymentID)
			if ferr != nil || h != deploymentID+"-app" || p+"" == "" {
				return ErrRetargetShape
			}
			if spec.Peer != deploymentID+"-app-next:"+p {
				return fmt.Errorf("standby port must match the upstream port")
			}
			out = append(out, standbyPairLines(line[len("  reverse_proxy "):], spec)...)
			continue
		}
		out = append(out, line)
	}
	if err := writeSwitchFile(path, []byte(strings.Join(out, "\n"))); err != nil {
		return fmt.Errorf("write routing fragment: %w", err)
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
