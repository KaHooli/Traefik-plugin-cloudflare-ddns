# Traefik Cloudflare DNS & Tunnel Sync

A Traefik **provider plugin** that publishes the hostnames of your Traefik routers to
Cloudflare. It reads Traefik's own API, so it sees hostnames from Docker labels, the file
provider and `defaultRule` alike. It then keeps a DNS record for each one pointing at your
public IP (DDNS), and removes the records it created once they are no longer needed.

> **Status: Phase 1 (DDNS).** Cloudflare Tunnel mode is planned for Phase 2. Hosts on
> tunnel entrypoints are discovered and logged, but not published yet. See [PLAN.md](PLAN.md).

## How it decides what to do

1. **Discover:** every enabled HTTP router's `Host(...)` names, from all providers.
   `HostRegexp` and wildcards are skipped.
2. **Mode by entrypoint:** each host gets the mode of the entrypoints its routers use,
   via `entryPointModes` (unlisted entrypoints use `defaultMode`):
   - `ddns`: an `A` record pointing at the public IPv4 address
   - `tunnel`: Cloudflare Tunnel (Phase 2; currently skipped)
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
     else is left alone and logged, unless you set `adopt: true` (`A` records only;
     `CNAME`/`AAAA` are never replaced).
5. **Prune:** when a host disappears from Traefik, or its entrypoint moves to mode `none`,
   the record this instance created is deleted after `pruneGrace` (15 min). The wait covers
   container restarts and Traefik loading its providers at startup. Nothing is pruned
   while discovery returns no hosts at all.

Cloudflare is only called when something changed (the host list, modes, or public IP),
when the last attempt failed, when a pending deletion is due, or every `verifyInterval`.

## Setup

1. Create a Cloudflare API token with **Zone → Zone → Read** and **Zone → DNS → Edit**,
   for all zones or just the ones you want managed.

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
           tunnel: tunnel
           lan: none
         exclude:
           - "mail.example.com"
           - "*.mx.example.com"
         cloudflare:
           apiTokenFile: /run/secrets/cf_api_token
   ```

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
| `adopt` | `false` | Take over existing `A` records without the marker |
| `prune` | `true` | Delete owned records whose host is gone |
| `pruneGrace` | `15m` | How long a host must be gone before deletion |
| `entryPoints` / `providers` | all | Only *discover* routers on these entrypoints / from these providers |
| `traefikApi.url` | `http://127.0.0.1:8080/api` | Traefik API base URL |
| `traefikApi.username` / `password` | — | Basic auth for the API |
| `traefikApi.insecureSkipVerify` | `false` | For an `https` API URL with a self-signed cert |
| `traefikApi.timeout` | `10s` | Per-request timeout |
| `cloudflare.apiToken` / `apiTokenFile` | — | API token, or a file containing it (e.g. a Docker secret) |
| `cloudflare.zones` | all visible | Allow-list of zone names |
| `cloudflare.instanceId` | `traefik` | Ownership marker; use different values for several Traefik hosts on one account |
| `ddns.ipSources` | ipify, AWS, ifconfig.me | Plain-text public-IPv4 URLs, tried in order |
| `ddns.staticIp` | — | Use this IPv4 instead of detecting it |
| `ddns.ipInterval` | `5m` | How often to re-detect the public IP (min `30s`) |
| `ddns.proxied` | `true` | Cloudflare proxy (orange cloud) on records |
| `ddns.ttl` | `1` (auto) | TTL for unproxied records (`30`–`86400`) |

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
