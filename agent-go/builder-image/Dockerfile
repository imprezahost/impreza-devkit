FROM golang:1.26.6-alpine3.23@sha256:e57c41c1d5864341031181b0db34b9a537bb5773eb6428e4e5bdaea0f9135406 AS compile
RUN apk add --no-cache git build-base libseccomp-dev libseccomp-static
ENV GOTOOLCHAIN=local GOFLAGS=-mod=mod GOMAXPROCS=2
WORKDIR /src
RUN go mod init build.invalid/tools && go mod download github.com/moby/buildkit@v0.33.0 github.com/rootless-containers/rootlesskit/v3@v3.0.1 github.com/opencontainers/runc@v1.4.3 github.com/containernetworking/plugins@v1.9.1
RUN cp -a /go/pkg/mod/github.com/moby/buildkit@v0.33.0 /src/buildkit && cp -a /go/pkg/mod/github.com/rootless-containers/rootlesskit/v3@v3.0.1 /src/rootlesskit && cp -a /go/pkg/mod/github.com/opencontainers/runc@v1.4.3 /src/runc && cp -a /go/pkg/mod/github.com/containernetworking/plugins@v1.9.1 /src/cni && chmod -R u+w /src && mkdir /out /licenses
WORKDIR /src/buildkit
RUN go get github.com/containerd/containerd/v2@v2.3.5 google.golang.org/grpc@v1.83.2 github.com/moby/go-archive@v0.3.0 golang.org/x/net@v0.58.0 golang.org/x/crypto@v0.56.0 golang.org/x/text@v0.41.0 && CGO_ENABLED=0 go build -p 2 -trimpath -ldflags '-w -X github.com/moby/buildkit/version.Version=v0.33.0-impreza.1 -X github.com/moby/buildkit/version.Package=github.com/moby/buildkit' -tags 'osusergo netgo static_build seccomp' -o /out/buildkitd ./cmd/buildkitd && CGO_ENABLED=0 go build -p 2 -trimpath -ldflags '-w -X github.com/moby/buildkit/version.Version=v0.33.0-impreza.1' -o /out/buildctl ./cmd/buildctl && cp LICENSE /licenses/BuildKit && cp go.mod /licenses/buildkit-go.mod && cp go.sum /licenses/buildkit-go.sum
WORKDIR /src/rootlesskit
RUN go get golang.org/x/net@v0.58.0 golang.org/x/crypto@v0.56.0 golang.org/x/text@v0.41.0 && CGO_ENABLED=0 go build -p 2 -trimpath -ldflags '-w' -o /out/rootlesskit ./cmd/rootlesskit && cp LICENSE /licenses/RootlessKit && cp go.mod /licenses/rootlesskit-go.mod && cp go.sum /licenses/rootlesskit-go.sum
WORKDIR /src/runc
RUN go get github.com/opencontainers/cgroups@v0.1.0 github.com/cilium/ebpf@v0.22.0 golang.org/x/net@v0.58.0 golang.org/x/crypto@v0.56.0 golang.org/x/text@v0.41.0 && CGO_ENABLED=1 go build -p 2 -trimpath -ldflags '-w -extldflags -static' -tags 'apparmor seccomp netgo cgo static_build osusergo' -o /out/buildkit-runc ./ && cp LICENSE /licenses/runc && cp go.mod /licenses/runc-go.mod && cp go.sum /licenses/runc-go.sum
WORKDIR /src/cni
RUN go get golang.org/x/net@v0.58.0 golang.org/x/crypto@v0.56.0 golang.org/x/text@v0.41.0 && CGO_ENABLED=0 go build -p 2 -trimpath -ldflags '-w' -o /out/buildkit-cni-bridge ./plugins/main/bridge && CGO_ENABLED=0 go build -p 2 -trimpath -ldflags '-w' -o /out/buildkit-cni-loopback ./plugins/main/loopback && CGO_ENABLED=0 go build -p 2 -trimpath -ldflags '-w' -o /out/buildkit-cni-host-local ./plugins/ipam/host-local && CGO_ENABLED=0 go build -p 2 -trimpath -ldflags '-w' -o /out/buildkit-cni-firewall ./plugins/meta/firewall && cp LICENSE /licenses/CNI && cp go.mod /licenses/cni-go.mod && cp go.sum /licenses/cni-go.sum
# Retain ELF symbols in shipped binaries for precise binary auditing.
# Preserve exact dependency graphs and audit every executable, including symbols.
RUN cd /src/buildkit && CGO_ENABLED=0 go list -deps -tags 'osusergo netgo static_build seccomp' ./cmd/buildkitd ./cmd/buildctl > /licenses/buildkit-packages.txt && cd /src/rootlesskit && CGO_ENABLED=0 go list -deps ./cmd/rootlesskit > /licenses/rootlesskit-packages.txt && cd /src/runc && CGO_ENABLED=1 go list -deps -tags 'apparmor seccomp netgo cgo static_build osusergo' ./ > /licenses/runc-packages.txt && cd /src/cni && CGO_ENABLED=0 go list -deps ./plugins/main/bridge ./plugins/main/loopback ./plugins/ipam/host-local ./plugins/meta/firewall > /licenses/cni-packages.txt && ! grep -E '^golang.org/x/crypto/openpgp(/|$)' /licenses/*-packages.txt
RUN GOBIN=/audit go install golang.org/x/vuln/cmd/govulncheck@v1.8.0 && for binary in /out/*; do /audit/govulncheck -mode=binary "$binary" > "/licenses/$(basename "$binary").govulncheck.txt" || { cat "/licenses/$(basename "$binary").govulncheck.txt"; exit 1; }; done
FROM moby/buildkit@sha256:80b15f0735e87bab7bf59ec4d695dfb4a7cfb25521cf56dc75d6f256285b63ef AS runtime
USER 0:0
RUN apk upgrade --no-cache
COPY --from=compile /out/ /usr/bin/
COPY --from=compile /licenses/ /usr/share/licenses/impreza-builder/
FROM scratch
COPY --from=runtime / /
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin HOME=/home/user USER=user XDG_RUNTIME_DIR=/run/user/1000 TMPDIR=/home/user/.local/tmp BUILDKIT_HOST=unix:///run/user/1000/buildkit/buildkitd.sock
USER 1000:1000
VOLUME ["/home/user/.local/share/buildkit"]
ENTRYPOINT ["rootlesskit", "buildkitd"]
LABEL org.opencontainers.image.title="Impreza controlled build executor" org.opencontainers.image.version="0.33.0-impreza.1" org.opencontainers.image.source="https://github.com/imprezahost/impreza-devkit"
