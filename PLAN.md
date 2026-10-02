# Traefik Cloudflare DDNS & Tunnel Plugin — Project Plan

Status: **draft / for review** · Target: Traefik v3.x · Language: Go (Yaegi-interpreted)

## 1. Goal

A Traefik plugin that keeps Cloudflare in sync with the hostnames Traefik is serving.
It reads every hostname Traefik knows about, from Docker labels **and** from the file
provider (including hostnames that only exist because of a provider's `defaultRule`),
and for each one makes sure Cloudflare has either:

- a **DDNS record** (`A`, and optionally `AAAA`) pointing at the current public IP, or
- a **Cloudflare Tunnel route**: a proxied `CNAME` to `<tunnel-id>.cfargotunnel.com` plus,
  for remotely managed tunnels, an ingress rule in the tunnel configuration.

Requirements from the brief:

| # | Requirement | How the design meets it |
|---|---|---|
| R1 | Hostnames from Docker container labels | Read from Traefik's API (`/api/http/routers`), which already contains Docker-provider routers |
| R2 | Hostnames from Traefik's file config, including defaults | The same API call returns file-provider routers, and routers whose rule came from `defaultRule` appear with the rule already filled in |
| R3 | DDNS **and** tunnel | Each hostname is assigned a *target mode* (`ddns` / `tunnel`) from the entrypoints its routers use |
| R4 | Several zones/TLDs in one Cloudflare account | Zones are found automatically by longest-suffix match over every zone the token can see |
| R5 | Skip some subdomains that already point somewhere else | An explicit `exclude` list, **and** an ownership marker so the plugin never overwrites a record it did not create |
| R6 | Remove subdomains for the target that are no longer needed | `prune` (on by default) deletes records this instance created once their host has been gone for `pruneGrace` |

## 2. Lessons from the reference plugin

[`XDSORITE/cloudflare-ddns-traefik-plugin`](https://github.com/XDSORITE/cloudflare-ddns-traefik-plugin)
is a **middleware** plugin. Its limitations are the reasons for this project:

1. **It only sees what it is told.** A middleware gets its own config, not the router it is
   attached to. Hostnames have to be copied into `routerRule:` / `domains:` by hand, so
   `defaultRule` hosts and file-provider routers are never found on their own.
2. **It has to be attached to routers** to be instantiated at all, and the first instance's
   token/zone "wins" for a process-wide singleton.
3. **It supports a single `zone`** (otherwise it uses best-match without allow-listing),
   only handles `A` records, has no tunnel support, and no delete/cleanup.
4. **It overwrites any existing `A` record** for the name, which would clobber the
   "already created with a different target" subdomains (R5).

Worth keeping: the passive design (DNS work never blocks requests), the ordered list of IP
sources with fallback, and the `managed-by` comment on created records.

## 3. Architecture

### 3.1 Plugin type: **provider plugin** (not middleware)

Traefik supports two Yaegi plugin types. A *provider* plugin starts once at boot from
**static** config and runs for as long as Traefik does. That fits a background sync daemon:

```go
// Required by Traefik (verified against traefik/pkg/plugins/providers.go)
func CreateConfig() *Config
func New(ctx context.Context, config *Config, name string) (*Provider, error)
func (p *Provider) Init() error
func (p *Provider) Provide(cfgChan chan<- json.Marshaler) error // start goroutine, return
func (p *Provider) Stop() error
```

The provider **does not have to send any dynamic configuration**. It only uses the
lifecycle hooks to run its sync loop. (Optional later: publish a no-op `ddns-status`
router/service for health. See §9.)

Static config (`traefik.yml`):

```yaml
experimental:
  plugins:
    cfsync:
      moduleName: github.com/KaHooli/Traefik-plugin-cloudflare-ddns
      version: v0.1.0

providers:
  plugin:
    cfsync:
      traefikApi:
        url: http://127.0.0.1:8080/api      # see §3.3
      cloudflare:
        apiTokenFile: /run/secrets/cf_token # or apiToken: "..." (file config does not expand ${VARS})
        accountId: "abc123..."              # needed for tunnel mode only
      # ... see §5 for full schema
```

### 3.2 Components

```
                ┌────────────────────────────────────────────────────────┐
 Traefik API ──►│ Discovery  ─► Host extraction ─► Filter (include/exclude)
 (routers)      │                                     │                   │
                │                          Mode resolver (ddns | tunnel)  │
 IP sources ───►│ Public-IP detector ─────────┐       │                   │
                │                             ▼       ▼                   │
                │             Desired state: {host, zone, type, content}  │
                │                             │                           │
 Cloudflare ◄──►│ Zone index ─► Reconciler (diff vs. actual, ownership)   │
  DNS + Tunnel  │                 │ create / update / (opt.) delete       │
                │                 └─► Tunnel ingress merger (remote cfg)  │
                └────────────────────────────────────────────────────────┘
```

| Package / file | Responsibility |
|---|---|
| `provider.go` | Traefik entry points (`CreateConfig`, `New`, `Init`, `Provide`, `Stop`), loop, back-off |
| `config.go` | Config struct, defaults, validation, `apiTokenFile` loading |
| `discovery.go` | Traefik API client, pagination, router → host list |
| `rule.go` | Parse `Host()` / `HostRegexp()` out of rule strings (v2 and v3 syntax) |
| `match.go` | Glob matching for `exclude`, entrypoint → mode resolution |
| `publicip.go` | IPv4/IPv6 detection with fallback sources |
| `cloudflare.go` | Minimal REST client: zones, DNS records, tunnel config (stdlib `net/http` only) |
| `reconcile.go` | Desired vs. actual diff, ownership checks, dry-run |
| `tunnel.go` | Merge managed ingress rules into the remote tunnel configuration |

> **Yaegi constraints:** stdlib only (or vendored pure-Go deps), no `unsafe`, `syscall`,
> cgo, or generics-heavy tricks. **Do not** import `cloudflare-go`; it is large, would have
> to be vendored, and is risky under Yaegi. A hand-written client for ~8 endpoints is simpler and
> testable with `httptest`.

### 3.3 Discovering hostnames via the Traefik API

`GET {traefikApi.url}/http/routers?per_page=100&page=N` returns every router from every
provider (Docker, file, KV…), with `rule`, `provider`, `entryPoints`, `status` and
`tls.domains`. This one source covers R1 and R2:

- **Docker labels:** `traefik.http.routers.app.rule=Host(\`app.example.com\`)` → router `app@docker`.
- **Docker `defaultRule`:** e.g. `defaultRule: "Host(\`{{ normalize .Name }}.example.com\`)"`.
  Containers with no explicit rule still produce a router whose evaluated `rule` is in the API.
- **File provider:** routers in `dynamic/*.yml` → `name@file`.
- **TLS domains:** `tls.domains[].main/sans` can also be harvested (optional, off by default;
  wildcards are skipped).

Extraction rules:

- `Host(\`a\`, \`b\`)` (v2 multi-arg) and `Host(\`a\`) || Host(\`b\`)` (v3) → `a`, `b`.
- `HostRegexp(...)`, wildcards and `HostSNI(\`*\`)` are **skipped** and logged once.
- Optional `tcp/routers` `HostSNI(\`x\`)` support (flag, default off).
- Only routers with `status: enabled` are considered (configurable).
- Filter by `entryPoints` and/or `provider` (e.g. only `websecure`, only `docker`/`file`).

**Making the API reachable:**

- Simplest: `api.insecure: true` and an `entryPoints.traefik` on `127.0.0.1:8080`.
  The plugin runs inside the Traefik process, so loopback works and nothing is exposed.
- Alternatively a dedicated internal entrypoint + `api@internal` router, with optional
  basic-auth (`traefikApi.username/password`) and `insecureSkipVerify`.
- **Startup race:** the API is not ready when `Provide` is called. The loop waits with
  back-off (1s → 30s) until the API answers, and is also *debounced*: a run happens at most
  every `minInterval` (e.g. 15s) after router changes, plus a full run every `interval`.

Fallback/alternative source (phase 3, optional): talk to the Docker socket directly
(`unix:///var/run/docker.sock`) to read **custom per-container labels** such as
`cfsync.mode=tunnel` / `cfsync.skip=true`. Traefik's API does not expose arbitrary labels,
so this is the only way to get per-container metadata. Until then, per-host behaviour is
driven by entrypoint modes and `exclude` in the plugin config.

## 4. Cloudflare behaviour

### 4.1 Zones (R4)

- On each cycle (cached, refreshed every `zoneRefresh`, default 1h): `GET /zones?per_page=50`
  over all pages. The token can see every zone in the account it has access to.
- Each host is assigned the **longest matching zone name** (`a.b.example.co.uk` →
  `example.co.uk`, not `co.uk`). Works across TLDs without extra config: `app.example.com`
  and `app.example.net` resolve to their own zones.
- Optional `zones: [..]` allow-list. Hosts in zones not on the list are skipped.
- Hosts with no matching zone are logged once per change (not every cycle) and skipped.

### 4.2 DDNS mode

- Public IPv4 from ordered `ipSources` (default: `https://api.ipify.org`,
  `https://checkip.amazonaws.com`, `https://ifconfig.me/ip`), validated with `net.ParseIP`.
  Optional IPv6 (`ipv6: true`, sources like `https://api6.ipify.org`) → `AAAA`.
- Optional `staticIp` override (skip detection).
- Record: `A <host> → <ip>`, `proxied` from rule/default, `ttl: 1` (auto), and
  `comment: "managed-by=traefik-cfsync instance=<instanceId>"`.
- Optional **CNAME-flattening shortcut** (`ddnsTarget: home.example.com`): create one `A`
  record for an anchor host and `CNAME` everything else to it. That means one update when
  the IP changes instead of N, and fewer API calls.

### 4.3 Tunnel mode

For hosts routed to a tunnel:

1. **DNS:** `CNAME <host> → <tunnelId>.cfargotunnel.com`, `proxied: true` (required).
2. **Ingress (remotely managed tunnels only, `tunnel.manageIngress: true`):**
   `GET/PUT /accounts/{accountId}/cfd_tunnel/{tunnelId}/configurations`
   - Add `{hostname: <host>, service: <tunnel.service>}`. `service` is Traefik's tunnel
     entrypoint as cloudflared reaches it (e.g. `http://traefik:8081`), overridable per
     entrypoint; `https://` services also get `originRequest.originServerName: <host>`.
   - **Merge, never replace:** rules for other hosts, path rules, unknown fields and other
     tunnel settings are kept as they are. New rules go before the first wildcard rule; the
     catch-all stays **last**. Write only when the merged result differs (comparison ignores
     the `originRequest: {}` Cloudflare adds).
   - **Ownership:** a rule is the plugin's to change or remove only when the host's `CNAME`
     carries this instance's marker. It is removed only in the same pass that deletes that
     `CNAME`, so no separate state is needed. Ingress is written before DNS; if the write
     fails, the `CNAME` deletions are held back and retried. A differing rule the plugin
     doesn't own blocks the host (unless `adopt: true`).
3. **Locally managed tunnels** (`config.yml` in cloudflared): only step 1 is possible. Document
   pointing cloudflared's ingress at Traefik with a wildcard so Traefik does the routing.

Required token permissions:
`Zone:Read`, `DNS:Edit` (all zones, or the specific zones), and for tunnel ingress
`Account: Cloudflare Tunnel:Edit`.

### 4.4 Mode selection (decided: by entrypoint)

Each entrypoint maps to a mode; entrypoints not listed use `defaultMode`:

```yaml
defaultMode: ddns            # ddns | tunnel | none
entryPointModes:
  tunnel: tunnel             # put tunnel-only services on a dedicated entrypoint
  lan: none                  # discovered but never published
```

A host takes the mode of the entrypoints its routers use. If those entrypoints map to
**different** modes (e.g. one router on `websecure`, another on `tunnel`), the host is
logged as a conflict and left untouched. Hostname-pattern rules and per-container labels
were considered and not chosen; `exclude` still covers per-host opt-outs.

### 4.5 Skipping & ownership safety (R5)

Two layers, so a mistake in one doesn't damage existing DNS:

1. **Explicit exclude** (checked first; matching hosts are never touched):
   ```yaml
   exclude:
     - "mail.example.com"
     - "*.mx.example.com"
     - "legacy.example.net"
   ```
2. **Ownership guard** (on by default, `adopt: false`): if a record with that name already
   exists **without** the plugin's `managed-by` comment and with a different
   type/content, the plugin **leaves it alone** and logs `skipped: foreign record`.
   - `adopt: true` lets the plugin take over records it didn't create,
     and adds the comment.
   - A `CNAME` and an `A` cannot coexist for the same name. The guard also stops a
     DDNS↔tunnel mode switch from breaking a foreign record.

**Deletion (R6)** (`prune: true` by default): records carrying this instance's
`managed-by` comment are deleted once their host has been gone from Traefik, or moved to
mode `none`, for longer than `pruneGrace` (default 15m). Safeguards:
- only owned records are deleted; `exclude` matches and foreign records never are
- the grace period covers container restarts and Traefik loading providers at startup
  (a partial router list), and restarts from zero when Traefik restarts
- nothing is pruned while discovery returns no hosts at all
- conflicting hosts are kept

Owned tunnel ingress rules are removed together with their `CNAME`. The `instanceId` in the
comment lets several Traefik hosts share one Cloudflare account safely.

### 4.6 API efficiency & rate limits

Cloudflare allows ~1,200 requests / 5 min per token.

- One `GET /zones/{id}/dns_records?per_page=5000` per *relevant* zone per cycle (not per host).
- Diff locally; write only changes. Use the `/dns_records/batch` endpoint where available.
- When the IP and router set haven't changed since the last good cycle, skip the Cloudflare
  read entirely and only do a full verify every `verifyInterval` (default 1h).
- Back off on `429`/`5xx`.

## 5. Configuration schema (draft)

```yaml
providers:
  plugin:
    cfsync:
      interval: 5m             # full reconcile period
      minInterval: 15s         # debounce for router changes
      dryRun: false            # log intended changes only
      logLevel: info

      traefikApi:
        url: http://127.0.0.1:8080/api
        username: ""
        password: ""
        insecureSkipVerify: false
        entryPoints: []        # filter; empty = all
        providers: []          # e.g. [docker, file]; empty = all
        includeTlsDomains: false
        includeTcpRouters: false

      cloudflare:
        apiToken: ""
        apiTokenFile: ""
        accountId: ""
        zones: []              # allow-list; empty = all visible
        instanceId: "traefik"  # ownership marker suffix

      ddns:
        ipv4: true
        ipv6: false
        ipSources: []          # defaults built in
        ipv6Sources: []
        staticIp: ""
        proxied: true
        ttl: 1
        cnameTarget: ""        # optional anchor host

      tunnel:
        id: ""                 # tunnel UUID
        manageIngress: false   # true for remotely-managed tunnels
        service: "https://traefik:443"
        originServerName: true # set originRequest.originServerName=<host>
        noTLSVerify: false

      defaultMode: ddns
      entryPointModes: {}      # see §4.4
      exclude: []              # see §4.5
      adopt: false
      prune: true
      pruneGrace: 15m
```

`.traefik.yml` manifest:

```yaml
displayName: Cloudflare DNS & Tunnel Sync
type: provider
import: github.com/KaHooli/Traefik-plugin-cloudflare-ddns
summary: Publish Traefik router hostnames to Cloudflare as DDNS records or Tunnel routes, across multiple zones.
testData:
  dryRun: true
  traefikApi:
    url: http://127.0.0.1:8080/api
  cloudflare:
    apiToken: test
```

> Note: the module path should be lowercase-friendly. Consider renaming the repo to
> `traefik-plugin-cloudflare-ddns` (or using a lowercase module path) because the plugin
> catalog and Go module proxies are case-sensitive.

## 6. Repository layout

```
.traefik.yml
go.mod                    # module github.com/kahooli/traefik-plugin-cloudflare-ddns, go 1.22+
provider.go  config.go  discovery.go  rule.go  match.go
publicip.go  cloudflare.go  reconcile.go  tunnel.go
*_test.go                 # table tests + httptest fakes for Traefik API & Cloudflare
vendor/                   # only if a dependency is ever added (must be committed)
examples/
  docker-compose.yml      # traefik + whoami + labels + plugin (local mode)
  traefik.yml
  dynamic/routers.yml
docs/
  configuration.md  cloudflare-token.md  tunnel.md
.github/workflows/
  ci.yml                  # gofmt, go vet, golangci-lint, go test -race, yaegi test
  release.yml             # tag → GitHub release
.golangci.yml
README.md  LICENSE
```

## 7. Delivery phases

**Phase 0: Spike (½ day)**: ✅ done, see `docs/spike-results.md`
- Minimal provider plugin in local mode (`experimental.localPlugins`, mounted at
  `/plugins-local/src/github.com/...`) that logs the router list from the API.
- Confirm: Yaegi runs `net/http` + goroutines in a provider; the loopback API is reachable;
  behaviour when `Provide` sends no configuration.

**Phase 1: MVP DDNS (R1, R2, R4, R5, R6)**: ✅ done
- Discovery + rule parsing + entrypoint modes + exclude + zone index + A-record reconcile +
  ownership guard + prune with grace period + `apiTokenFile` + dry-run. Unit, fake-API
  end-to-end and Yaegi tests; verified inside Traefik v3.7.13 against a stand-in Cloudflare API.

**Phase 2: Tunnel mode (R3)**: ✅ done
- CNAME to `cfargotunnel.com`, remote ingress merge (foreign rules, wildcards, catch-all
  and other settings preserved), DNS-only mode for local tunnels, and safe DDNS↔tunnel
  switching of owned records. Ingress ownership comes from the owned CNAME, so it needs no
  extra state.

**Phase 3: Hardening & extras**: ✅ done
- Opt-in IPv6 (`AAAA`, with an `ipv4` switch for IPv6-only hosts); a family whose
  detection fails is left alone.
- `ddns.dnsOnlyEntryPoints` (grey cloud per entrypoint).
- TCP routers' `HostSNI` names (always DNS-only; tunnel mode refused) and routers'
  `tls.domains`, both opt-in.
- Tunnel rules stay managed while a tunnel is configured, so they are cleaned up even after
  tunnel mode is removed from every entrypoint.
- Deferred (not needed at current scale): CNAME anchor and batch API (fewer API calls on IP
  change), status endpoint (a provider plugin can't serve HTTP itself; logs cover it).

**Phase 4: Publish**
- `.traefik.yml` with valid `testData`, GitHub topic `traefik-plugin`, semver tag
  `v0.1.0`, and a README with icon/banner. The catalog picks it up automatically.

## 8. Testing strategy

- **Unit:** rule parser (v2/v3 syntax, `||`, `&&`, multi-arg, regex/wildcard skip), zone
  longest-suffix, glob matcher, ownership/diff decisions (table-driven), tunnel ingress merge
  (catch-all stays last, foreign rules untouched).
- **Contract:** `httptest` fakes for the Traefik API (paginated JSON from a real Traefik) and
  for Cloudflare (zones, records, tunnel configurations).
- **Yaegi:** `yaegi test -v .` in CI, because code that compiles with `go` can still fail in Yaegi.
- **E2E (manual/nightly):** `examples/docker-compose.yml` with `dryRun: true` against a
  real throwaway zone.

## 9. Risks & open questions

| Topic | Notes |
|---|---|
| Provider plugin maturity | Provider plugins are less widely used than middleware. Phase 0 checks this early; the fallback is shipping the same code as a small sidecar binary (`cmd/cfsync`) that reads the same API. |
| Secrets in static config | Traefik's static file config does not expand `${VARS}`, and env vars/CLI flags are ignored when a config file is used (verified). Use `apiTokenFile` (Docker secret). |
| Proxied vs. DNS-only | Orange-cloud breaks non-HTTP(S) ports. Default `proxied: true` for HTTP routers, global `ddns.proxied` for now; per-entrypoint override planned for Phase 3. |
| Multiple Traefik instances | `instanceId` in the comment keeps them from pruning each other's records. |
| Status visibility | Optionally expose a small JSON status via a dynamic router (`/cfsync/status`), or log only. |

**Questions for you:**

1. **Alternate TLD:** do hosts simply *live* in other zones (e.g. `app.example.net` is its
   own router), or should each host be **mirrored** across zones (router has
   `app.example.com` → also publish `app.example.org`)? The design covers the first. Mirroring
   would be a small extra feature (`mirrorZones`).
2. ~~Tunnel type~~: **remotely managed** (decided); `manageIngress: false` covers local tunnels.
3. ~~DDNS vs. tunnel split~~: **by entrypoint** (decided).
4. ~~Prune~~: **yes**, records no longer needed are removed (decided, R6).
5. ~~IPv6~~: not needed for the original deployment; planned as an opt-in for other users.
