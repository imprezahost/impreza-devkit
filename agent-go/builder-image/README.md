# Controlled build executor

This image provides the rootless BuildKit executor used by Impreza Agent's
optional controlled builds on Ubuntu 24.04 amd64. It is not an application
runtime image and is never started with the host Docker socket mounted.

The Dockerfile starts from BuildKit 0.33.0 and rebuilds BuildKit, RootlessKit,
runc and the CNI plugins with the dependency versions declared in the file.
The runtime filesystem is flattened so superseded executable bytes are not
retained in layers of the distributed archive. Source module manifests and
licenses are included under `/usr/share/licenses/impreza-builder/`. The shipped
executables retain ELF symbols for binary vulnerability checks. Exact package
graphs and binary audit results are included in that directory as well.

The module-level GO-2026-5932 warning applies to the deprecated
`golang.org/x/crypto/openpgp` packages. The build refuses those packages in its
dependency graphs and separately audits each executable. This does not suppress
other findings for `golang.org/x/crypto`.

Build on Linux amd64:

```sh
docker build --platform linux/amd64 -t impreza-controlled-builder .
```

The released archive is distributed through the agent-public repository,
alongside a SHA-256 checksum and a CycloneDX software bill of materials.
The agent has a compiled archive checksum and immutable image identities.
It rejects an unrecognized image even when its tag matches. Rebuilding this
Dockerfile does not authorize that image for an installed agent: package
indexes can change, so a new artifact must be audited, pinned and released.

To use the released image, install the supported agent and explicitly run
`sudo impreza-agent builder prepare`. This verifies the host prerequisites,
downloads and verifies the archive when needed, and enables controlled builds
for subsequent deployments. No fleet-wide activation is performed.
