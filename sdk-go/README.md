# Impreza Host Go SDK

> Hand-written Go client for the Impreza Host public REST API. Used by
> the official `impreza` CLI and the `impreza-agent` daemon, and
> available standalone for third-party integrations.

## Install

```bash
go get github.com/imprezahost/impreza-devkit/sdk-go@latest
```

## Quick start

```go
package main

import (
    "context"
    "fmt"

    sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
    sdkconfig "github.com/imprezahost/impreza-devkit/sdk-go/config"
)

func main() {
    cli, err := sdkclient.New(sdkconfig.Context{
        Key:    "imp_...",
        Secret: "...",
    })
    if err != nil {
        panic(err)
    }

    var info sdkclient.AccountInfo
    if err := cli.AccountInfo(context.Background(), &info); err != nil {
        panic(err)
    }
    fmt.Println(info.Email, info.Balance)
}
```

## Packages

| Package | Purpose |
|---|---|
| `client` | HTTP client + every resource method (account, vps, domain, dns, dedicated, orders, invoices, topup, webhooks, agent, platform). |
| `config` | TOML config loader. Multi-context, XDG paths. |
| `tor` | SOCKS5 dialer helper for routing requests through Tor or any SOCKS5 proxy. |

## Tor / SOCKS5

Set `Proxy` (any SOCKS5 URL) or `UseTor` (defaults to `socks5://127.0.0.1:9050`) on the `Context` passed to `client.New`. Everything else is automatic — retries, auth headers, error decoding all flow through the proxy.

```go
cli, _ := sdkclient.New(sdkconfig.Context{
    Key:    "imp_...",
    Secret: "...",
    UseTor: true,
})
```

## Compatibility

| Surface | Status |
|---|---|
| Public REST (`/v1/account/...`, `/v1/vps/...`, `/v1/domain/...`, etc.) | Stable, GA |
| Webhook signature verification | Stable, GA |
| Agent protocol (`/v1/agent/...`) | **Draft** — schemas may change before GA |
| Platform endpoints (`/v1/platform/...`) | **Draft** — schemas may change before GA |

## License

Proprietary — see [`LICENSE`](../LICENSE).
