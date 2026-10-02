# Traefik Cloudflare DNS & Tunnel Sync

A Traefik **provider plugin** that publishes the hostnames of your Traefik routers to
Cloudflare, as DDNS records or Cloudflare Tunnel routes. It finds hostnames from Docker
labels, the file provider and `defaultRule` by reading Traefik's own API.

> **Status: Phase 0 spike.** The plugin discovers and logs hostnames only. Nothing is
> written to Cloudflare yet. See [PLAN.md](PLAN.md) for the design and
> [docs/spike-results.md](docs/spike-results.md) for what the spike established.

## Try it (local plugin mode)

1. Enable the API on a loopback entrypoint, so the plugin can read it from inside Traefik:

   ```yaml
   entryPoints:
     traefik:
       address: "127.0.0.1:8080"
   api:
     insecure: true
   ```

2. Mount this repository at
   `/plugins-local/src/github.com/kahooli/traefik-plugin-cloudflare-ddns` and add:

   ```yaml
   experimental:
     localPlugins:
       cfsync:
         moduleName: github.com/kahooli/traefik-plugin-cloudflare-ddns

   providers:
     plugin:
       cfsync:
         pollInterval: 30s
         traefikApi:
           url: http://127.0.0.1:8080/api
   ```

3. Watch Traefik's log for `[plugin-cfsync] discovered N host(s)`.

[`examples/`](examples) has a complete `docker-compose.yml`, static config and file-provider routers.

## Spike configuration

| Option | Default | Description |
|---|---|---|
| `pollInterval` | `30s` | How often to read the Traefik API (minimum `5s`) |
| `traefikApi.url` | `http://127.0.0.1:8080/api` | API base URL, including `/api` |
| `traefikApi.username` / `password` | — | Basic auth, if the API is behind it |
| `traefikApi.insecureSkipVerify` | `false` | For an `https` API URL with a self-signed cert |
| `traefikApi.timeout` | `10s` | Per-request timeout |
| `entryPoints` | all | Only consider routers on these entrypoints |
| `providers` | all | Only consider routers from these providers (`docker`, `file`, …) |

## Development

```sh
go test -race ./...
# Traefik runs plugins in the Yaegi interpreter; always also run:
go install github.com/traefik/yaegi/cmd/yaegi@v0.16.1
# from a GOPATH layout: $GOPATH/src/github.com/kahooli/traefik-plugin-cloudflare-ddns
GO111MODULE=off yaegi test -v .
```
