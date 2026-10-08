package egress

import (
	"context"
	"errors"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/ingress"
)

// dockerBridgeInterfaces lists the Linux bridge interfaces Docker itself
// owns — the same enumeration the ingress firewall trusts. The old shape
// rendered `br+`, which also matches bridges that are NOT Docker's: a host
// whose public interface is a bridge (br0) had its own INPUT traffic walk
// the container drops. Scoping to Docker's own bridges closes that; when
// the list cannot be read (Docker not answering), the conservative
// fallback is the default bridge only — never the wildcard.
//
// A bridge that simultaneously carries a default route is skipped: the
// egress drops are for container-facing bridges, and a default-route
// bridge is host-facing by definition (the ingress firewall refuses the
// same combination).
func dockerBridgeInterfaces(ctx context.Context, docker dockerRunner, previous []string) []string {
	fallback := []string{"docker0"}
	if len(previous) > 0 {
		fallback = previous // Docker not answering: keep the last scope applied
	}
	if docker == nil {
		return fallback
	}
	run := ingress.Runner(func(ctx context.Context, bin string, args []string, stdin string) ([]byte, error) {
		if bin != "docker" {
			return nil, errUnexpectedBinary
		}
		return docker(ctx, args...)
	})
	bridges, err := ingress.DockerBridges(ctx, run)
	if err != nil || len(bridges) == 0 {
		return fallback
	}
	defaults := map[string]bool{}
	for _, name := range ingress.DefaultRouteInterfaces() {
		defaults[name] = true
	}
	var out []string
	for _, b := range bridges {
		if defaults[b] {
			continue
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var errUnexpectedBinary = errors.New("egress bridge enumeration: unexpected binary")
