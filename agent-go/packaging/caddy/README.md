# Caddy image with the Impreza DNS provider

The image includes Caddy 2.11.4 standard modules and `dns.providers.impreza`.
The provider uses the Impreza API for DNS-01; customer servers do not receive
Cloudflare account credentials. The `-cf` image suffix is retained for compatibility.

Build from the repository root:

```sh
docker build -f agent-go/packaging/caddy/Dockerfile -t impreza-caddy:candidate .
docker run --rm impreza-caddy:candidate caddy version
docker run --rm impreza-caddy:candidate caddy list-modules
```

`application/go.mod` and `application/go.sum` pin the complete application graph, including
standard modules absent from the DNS plugin alone. The builder uses Go 1.26.6
and `-mod=readonly`. The requested image version must match the locked Caddy
module. Update the lockfiles deliberately and audit the full application before
changing a release. A plugin-only test does not audit the resulting server binary.

The release workflow publishes Linux amd64/arm64 images after verification.
Version tags and the compatibility streams `2-cf` and `latest-cf` are separate
from agent binaries. Existing servers require an explicit customer update;
creating a source commit does not distribute an updated image or agent.

Test the Impreza module, HTTPS and password-protected routing against the built
image, and inspect embedded dependencies. Vulnerability findings require review
of the actual calling paths; do not silently allow new findings in a release gate.
