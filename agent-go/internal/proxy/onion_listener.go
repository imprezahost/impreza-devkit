package proxy

import (
	"context"
	"fmt"
	"strings"
)

const onionBind = "  bind unix//config/onion-private/http.sock|0600"

// Legacy fragments are secured while assembling the active configuration.
// No network listener may serve a hidden service, even with a forged Host.
func secureOnionFragment(raw string) (string, error) {
	header, blocks, err := splitFragment(raw)
	if err != nil {
		return "", err
	}
	for i, block := range blocks {
		lines := strings.Split(block, "\n")
		first := strings.TrimSpace(lines[0])
		if !strings.HasPrefix(first, "http://") || !strings.HasSuffix(first, ".onion {") {
			continue
		}
		found := false
		for _, line := range lines[1:] {
			if strings.HasPrefix(strings.TrimSpace(line), "bind ") {
				if strings.TrimSpace(line) != strings.TrimSpace(onionBind) || found {
					return "", fmt.Errorf("unexpected onion listener binding")
				}
				found = true
			}
		}
		if !found {
			lines = append(lines[:1], append([]string{onionBind}, lines[1:]...)...)
		}
		blocks[i] = strings.Join(lines, "\n")
	}
	parts := append(header, blocks...)
	return strings.Join(parts, "\n") + "\n", nil
}

// ReconcileOnionListeners upgrades legacy fragments in the active Caddyfile.
// Called before starting Tor or applying authorization, never silently ignored.
func (c *Caddy) ReconcileOnionListeners(ctx context.Context) error {
	if err := c.guardRoutingSwitch(); err != nil {
		return err
	}
	if err := c.ensureDirs(); err != nil {
		return err
	}
	if err := c.regenerateCaddyfile(); err != nil {
		return err
	}
	if err := c.EnsureRunning(ctx); err != nil {
		return err
	}
	return c.reloadSwitch(ctx)
}
