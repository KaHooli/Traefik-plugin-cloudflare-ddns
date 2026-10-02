# Traefik Cloudflare DNS & Tunnel Sync

A Traefik **provider plugin** that publishes the hostnames of your Traefik routers to
Cloudflare. It reads Traefik's own API, so it sees hostnames from Docker labels, the file
provider and `defaultRule` alike. Each hostname is published either as a DNS record
pointing at your public IP (DDNS), or through a Cloudflare Tunnel, depending on the
entrypoint its router uses. Records and tunnel routes it created are removed once they
are no longer needed.

> **Status:** DDNS (IPv4) and Cloudflare Tunnel are implemented. IPv6 and other extras
> are planned; see [PLAN.md](PLAN.md).

## How it decides what to do

1. **Discover:** every enabled HTTP router's `Host(...)` names, from all providers.
   `HostRegexp` and wildcards are skipped.
2. **Mode by entrypoint:** each host gets the mode of the entrypoints its routers use,
   via `entryPointModes` (unlisted entrypoints use `defaultMode`):
   - `ddns`: an `A` record pointing at the public IPv4 address
   - `tunnel`: a proxied `CNAME` to `<tunnel-id>.cfargotunnel.com`, plus a public-hostname
     (ingress) rule on the tunnel sending the host to Traefik
   - `none`: never published

   If a host's routers use entrypoints with *different* modes, it is left untouched and
   logged as a conflict.
3. **Zone:** the longest Cloudflare zone name the host ends with, across every zone the
   token can see. So `app.example.com` and `app.example.net` each land in their own
   zone of the same account. Restrict this with `cloudflare.zones`.
4. **Safety:**
   - Hosts matching `exclude` are never touched.
   - Records are only changed or deleted if they carry this instance's marker comment
     (`managed-by=cfsync instance=<instanceId>`). An existing record pointing somewhere
     else is left alone and logged. `adopt: true` takes over existing `A` records and
     `CNAME`s that already point at the tunnel; anything else is never replaced.
   - An existing tunnel rule for the host that differs from what the plugin would write
     also blocks the host (unless `adopt: true`).
   - Changing a host's mode (moving its router to another entrypoint) swaps the `A` record
     and the tunnel `CNAME` + rule, but only for records this instance owns.
5. **Prune:** when a host disappears from Traefik, or its entrypoint moves to mode `none`,
   the record this instance created is deleted after `pruneGrace` (15 min), together with
   its tunnel rule. The wait covers container restarts and Traefik loading its providers at
   startup. Nothing is pruned while discovery returns no hosts at all.

Cloudflare is only called when something changed (the host list, modes, or public IP),
when the last attempt failed, when a pending deletion is due, or every `verifyInterval`.

### Tunnel ingress rules

For a remotely-managed tunnel (created in the dashboard or via the API, run with
`cloudflared tunnel run --token ...`), the plugin edits the tunnel's public-hostname rules
directly:

- It adds `{hostname, service}` for each tunnel host. `service` is `tunnel.service`, or
  `tunnel.entryPointServices.<entrypoint>`: Traefik's tunnel entrypoint as cloudflared
  reaches it, e.g. `http://traefik:8081`. For an `https://` service it also sets
  `originServerName` to the hostname, so Traefik serves the right certificate.
- New rules go before any wildcard rule, so the wildcard can't hide them, and before the
  catch-all, which always stays last.
- Your other rules, path rules, and all other tunnel settings are left exactly as they were.
- A rule is only removed together with the `CNAME` this instance owns for the host, so
  ownership survives restarts. The config is only written when something changed.

For a **locally-managed** tunnel (`config.yml`), set `tunnel.manageIngress: false`. The
plugin then only creates the `CNAME`s, and you route hostnames to Traefik in `config.yml`,
e.g. with a wildcard rule. The plugin warns if it detects a locally-managed tunnel while
`manageIngress` is on.

## Setup

1. Create a Cloudflare API token with **Zone → Zone → Read** and **Zone → DNS → Edit**,
   for all zones or just the ones you want managed. For tunnel mode, also add
   **Account → Cloudflare Tunnel → Edit**.

2. Enable the API on a loopback entrypoint, so the plugin can read it from inside Traefik:

   ```yaml
   entryPoints:
     traefik:
       address: "127.0.0.1:8080"
   api:
     insecure: true
   ```

3. Add the plugin to the static configuration. Start with `dryRun: true` and check the log:

   ```yaml
   experimental:
     plugins:
       cfsync:
         moduleName: github.com/kahooli/traefik-plugin-cloudflare-ddns
         version: v0.1.0   # once released; until then use localPlugins (below)

   providers:
     plugin:
       cfsync:
         dryRun: true
         entryPointModes:
           tunnel: tunnel        # an entrypoint only cloudflared connects to
           lan: none
         exclude:
           - "mail.example.com"
           - "*.mx.example.com"
         cloudflare:
           apiTokenFile: /run/secrets/cf_api_token
           accountId: "<account-id>"        # tunnel mode only
         tunnel:
           id: "<tunnel-uuid>"
           service: http://traefik:8081     # the tunnel entrypoint, as cloudflared sees it
   ```

   Then put a router on the `tunnel` entrypoint (e.g. the label
   `traefik.http.routers.app.entrypoints=tunnel`) to publish it through the tunnel.

   For local development, mount this repository at
   `/plugins-local/src/github.com/kahooli/traefik-plugin-cloudflare-ddns` and use
   `experimental.localPlugins.cfsync.moduleName` instead.
   [`examples/`](examples) has a complete `docker-compose.yml`.

Log lines are prefixed with `[plugin-cfsync]`:

```
discovered 5 host(s) from 6 router(s)
  ddns   files.example.com <- file-app@file
  ddns   legacy.example.com <- legacy@file
  tunnel tun.example.com <- tunnelled@file
public IPv4 is 203.0.113.10
create A files.example.com -> 203.0.113.10 (proxied=true) [zone example.com]
skip legacy.example.com: foreign A record exists (-> 192.0.2.50); set adopt: true to take it over
tunnel ingress: add app.example.com -> http://traefik:8081
create CNAME app.example.com -> 6ff42ae2-....cfargotunnel.com (proxied=true) [zone example.com]
temp.example.com no longer served; A 203.0.113.10 will be deleted in 15m0s
```

## Configuration

| Option | Default | Description |
|---|---|---|
| `dryRun` | `false` | Log changes without making them |
| `pollInterval` | `30s` | How often to read the Traefik API (min `5s`) |
| `verifyInterval` | `1h` | Full Cloudflare check even when nothing changed |
| `defaultMode` | `ddns` | Mode for entrypoints not in `entryPointModes` |
| `entryPointModes` | — | Map of entrypoint → `ddns` / `tunnel` / `none` |
| `exclude` | — | Hostname globs (`*` matches across dots) never touched |
| `adopt` | `false` | Take over existing `A` records, `CNAME`s to the tunnel, and differing tunnel rules |
| `prune` | `true` | Delete owned records whose host is gone |
| `pruneGrace` | `15m` | How long a host must be gone before deletion |
| `entryPoints` / `providers` | all | Only *discover* routers on these entrypoints / from these providers |
| `traefikApi.url` | `http://127.0.0.1:8080/api` | Traefik API base URL |
| `traefikApi.username` / `password` | — | Basic auth for the API |
| `traefikApi.insecureSkipVerify` | `false` | For an `https` API URL with a self-signed cert |
| `traefikApi.timeout` | `10s` | Per-request timeout |
| `cloudflare.apiToken` / `apiTokenFile` | — | API token, or a file containing it (e.g. a Docker secret) |
| `cloudflare.zones` | all visible | Allow-list of zone names |
| `cloudflare.accountId` | — | Account owning the tunnel (needed to manage ingress) |
| `cloudflare.instanceId` | `traefik` | Ownership marker; use different values for several Traefik hosts on one account |
| `ddns.ipSources` | ipify, AWS, ifconfig.me | Plain-text public-IPv4 URLs, tried in order |
| `ddns.staticIp` | — | Use this IPv4 instead of detecting it |
| `ddns.ipInterval` | `5m` | How often to re-detect the public IP (min `30s`) |
| `ddns.proxied` | `true` | Cloudflare proxy (orange cloud) on records |
| `ddns.ttl` | `1` (auto) | TTL for unproxied records (`30`–`86400`) |
| `tunnel.id` | — | Tunnel UUID; required when any entrypoint uses `tunnel` |
| `tunnel.manageIngress` | `true` | Maintain the tunnel's public-hostname rules (remotely-managed tunnels) |
| `tunnel.service` | — | Where cloudflared sends traffic, e.g. `http://traefik:8081` |
| `tunnel.entryPointServices` | — | Per-entrypoint override of `tunnel.service` |
| `tunnel.originServerName` | `true` | For `https://` services, send the hostname as TLS SNI |
| `tunnel.noTLSVerify` | `false` | For `https://` services, skip certificate verification |

Traefik's static file config doesn't expand `${VARS}`, and Traefik reads static config
from only one source (file, environment *or* CLI flags), so `TRAEFIK_PROVIDERS_PLUGIN_...`
variables are ignored when you use a config file. Prefer `apiTokenFile` with a Docker secret.

## Development

```sh
go test -race ./...
# Traefik runs plugins in the Yaegi interpreter; always also run:
go install github.com/traefik/yaegi/cmd/yaegi@v0.16.1
# from a GOPATH layout: $GOPATH/src/github.com/kahooli/traefik-plugin-cloudflare-ddns
GO111MODULE=off yaegi test -v .
```

Yaegi behaves differently from Go in a few ways that matter here. See
[docs/spike-results.md](docs/spike-results.md#yaegi-pitfalls-found).
