# impreza-agent — build & packaging
#
# This Makefile is for local dev. CI uses nfpm to produce .deb/.rpm
# artifacts; see packaging/nfpm.yaml.

VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     ?= dist/impreza-agent

# Linux is the only deploy target; the CGO-free static binary works
# across glibc / musl / alpine without surprises.
export CGO_ENABLED=0
export GOOS=linux
export GOARCH=amd64

.PHONY: build build-host vet test smoke clean install

build:
	@mkdir -p dist
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) .
	@echo "→ $(BIN)"

# Host-platform build for local dev (Windows / macOS / Linux).
# Useful for running `--version` / `doctor` on a laptop.
build-host:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS= GOARCH= go build -trimpath -ldflags="$(LDFLAGS)" -o dist/impreza-agent-host .
	@echo "→ dist/impreza-agent-host"

vet:
	go vet ./...

test:
	go test ./...

# Quick smoke — exercise the verbs that don't need a real control plane.
smoke: build-host
	./dist/impreza-agent-host --version
	./dist/impreza-agent-host --help

clean:
	rm -rf dist/

# Local install — useful for testing the systemd unit on a dev Linux box.
install: build
	install -D -m 0755 $(BIN) /usr/local/bin/impreza-agent
	install -D -m 0644 packaging/systemd/impreza-agent.service /etc/systemd/system/impreza-agent.service
	systemctl daemon-reload
	@echo "Installed. Configure with: impreza-agent bootstrap --token <bootstrap-token>"
