// Command impreza is the Go-based CLI for the Impreza Host public REST API.
//
// Co-released with the Python `impreza-cli` package, this binary mirrors
// the Python CLI's surface (read commands, write verbs, VPS sub-resources,
// crypto top-up, webhooks). Both can be installed side by side; pick by
// PATH ordering.
//
// Build with `make build`. Install to GOPATH/bin via `make install`.
// Distribution is via GitHub Releases (Linux/macOS/Windows, x86_64/arm64).
package main

import (
	"os"

	"github.com/imprezahost/impreza-devkit/cli-go/cmd"
)

// version is set via build-time ldflags:
//
//	go build -ldflags "-X main.version=0.1.0" .
//
// Falls back to "dev" for source-tree builds.
var version = "dev"

func main() {
	cmd.SetVersion(version)
	if err := cmd.Execute(); err != nil {
		// Cobra already printed the error; just propagate the non-zero
		// exit so shells see the failure.
		os.Exit(1)
	}
}
