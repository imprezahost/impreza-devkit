# Makefile for impreza-cli-go.
#
# Common targets:
#   make build      # compile a local binary at ./impreza
#   make test       # go test ./... with race detector
#   make lint       # gofmt + go vet
#   make gen        # regenerate Go types from openapi/openapi.yaml (Phase 7.2+)
#   make install    # build + place at $(GOPATH)/bin/impreza
#   make clean      # remove build artefacts
#
# Version is derived from the most recent `cli-go-v*` git tag in the
# parent repo. Falls back to "dev" for source-tree builds.

VERSION := $(shell git describe --tags --abbrev=0 --match='cli-go-v*' 2>/dev/null | sed 's/^cli-go-v//' || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# Repo-wide OpenAPI spec — the codegen target reads this in 7.2+.
OPENAPI_SPEC := ../openapi/openapi.yaml

# Where oapi-codegen writes its output. Gitignored.
GENERATED_DIR := ./internal/api

.PHONY: build test lint gen install clean help

help:
	@echo "impreza-cli-go targets:"
	@echo "  build           compile ./impreza"
	@echo "  test            go test ./... with -race"
	@echo "  lint            gofmt -l + go vet"
	@echo "  install         build + install to \$$GOPATH/bin"
	@echo "  snapshot        goreleaser local build (no upload), all 5 platforms"
	@echo "  release-check   goreleaser config syntax check"
	@echo "  clean           remove build artefacts"
	@echo "  VERSION         currently $(VERSION)"

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o impreza .

test:
	go test -race -count=1 ./...

lint:
	@gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...

gen:
	@echo "Phase 7.2+: this target will run oapi-codegen to regenerate $(GENERATED_DIR) from $(OPENAPI_SPEC)."
	@echo "Not implemented yet — the Phase 7.1 bootstrap doesn't depend on the generated client."
	@exit 0

install:
	go install -trimpath -ldflags "$(LDFLAGS)" .

# Local goreleaser dry run — builds all 5 platform binaries under
# ./dist/ but does NOT upload to GitHub. Useful for validating config
# changes before tagging a real release.
snapshot:
	goreleaser release --snapshot --clean --skip=publish

# Validate .goreleaser.yml syntax without building anything.
release-check:
	goreleaser check

clean:
	rm -f impreza impreza.exe
	rm -rf dist/ build/ $(GENERATED_DIR)/
	# Release-prep assets copied here by scripts/prep_assets.go.
	# Source of truth stays at the repo root.
	rm -f LICENSE openapi.yaml asyncapi.yaml
