# Phase 0 spike: results

Date: 2026-10-02 · Traefik **v3.7.13** (official linux/amd64 binary) · Yaegi **v0.16.1**
(the version Traefik v3.7 vendors).

## Questions and answers

| Question | Result |
|---|---|
| Does a Yaegi **provider** plugin load from `experimental.localPlugins`? | **Yes.** `type: provider` with `basePkg: cfsync` in `.traefik.yml` loads, and Traefik calls `New` → `Init` → `Provide`. The plugin logs with a `[plugin-cfsync]` prefix. |
| Can the plugin make HTTP calls and run goroutines under Yaegi? | **Yes.** `net/http` client, `context`, timers and a background goroutine all work. |
| Is Traefik's API reachable from inside the plugin? | **Yes**, at `http://127.0.0.1:8080/api` with `api.insecure: true` on a loopback-only `traefik` entrypoint. Nothing is exposed outside the host/container. |
| Is the API ready when `Provide` is called? | **No.** The first request gets **HTTP 404** (the `api@internal` router isn't configured yet). One retry about 1 s later succeeded. Retrying with back-off is required. |
| Does the API include file-provider routers? | **Yes**, including multi-host `Host(a) \|\| Host(b)` rules; a router added to the watched file while running was picked up on the next poll. |
| Does it include Docker-label and `defaultRule` routers? | **Not tested here** (no Docker daemon in the spike environment). Traefik's API returns the evaluated rule for every provider, so this is expected to work. `examples/docker-compose.yml` is set up to confirm it (`blog` relies on `defaultRule`). |
| Must a provider plugin send a dynamic configuration? | **No.** Sending nothing is fine. Sending an empty `{}` was also accepted silently and other routers were unaffected. The plugin now sends nothing. |
| Is `Stop` called on shutdown? | **Yes**, on SIGTERM; the loop exits promptly. |

Run log (trimmed):

```
INF Plugins loaded. plugins=["cfsync"]
[plugin-cfsync] init: api=http://127.0.0.1:8080/api pollInterval=5s
INF Starting provider *plugins.Provider
[plugin-cfsync] waiting for Traefik API (retry in 1s): GET /http/routers: HTTP 404: 404 page not found
[plugin-cfsync] discovered 3 host(s) from 5 router(s)
  api.example.org <- file-api@file
  files.example.com <- file-app@file
  files.example.net <- file-app@file
  skipped non-literal matchers in: file-wildcard@file
[plugin-cfsync] discovered 4 host(s) from 6 router(s)
  ...
  late.example.com <- file-late@file
INF Shutting down
[plugin-cfsync] stopped
```

## Yaegi pitfalls found

Both passed `go test` and failed only under `yaegi test`, which is why CI runs both.

1. **Named result variables keep their value between calls.** With
   `func f() (out []string, ok bool)`, `out` is not reset to `nil` on the next call, so
   `append` adds to the previous call's result. In this plugin that would have made the
   host list grow on every poll. **Rule: no named results in plugin code.** It is
   enforced by the `nonamedreturns` linter and covered by `TestCollectHostsRepeatable`.
   Minimal repro:

   ```go
   func named(n int) (out []string, flag bool) {
       for i := 0; i < n; i++ { out = append(out, fmt.Sprint(i)) }
       return out, n == 0
   }
   // yaegi: named(2), named(0), named(1) → [0 1], [0 1], [0 1 0]
   ```

2. **Sending a plugin-defined type through a `select` on `chan<- json.Marshaler` panics**
   (`reflect.Select: value of type *struct {} is not assignable to type json.Marshaler`).
   Fix: send a stdlib type such as `json.RawMessage`. This matters in Phase 3 if the plugin
   ever publishes a status router.

3. **`fmt` does not call `String()` on interpreted types** (found in Phase 1). `%s` on a
   plugin type with a `String()` method prints interpreter internals
   (`{%!s(*interp.node=...)}`) instead. **Rule: call such methods explicitly**
   (`a.describe()`), and don't name them `String` so it's clear they won't be called
   implicitly.

4. **Method values of compiled types can't be passed as `func()`** in test code:
   `t.Cleanup(srv.Close)` fails with `cannot use type func(*httptest.Server) as type func()`.
   Use `t.Cleanup(func() { srv.Close() })`.

## Decisions for Phase 1

- Keep the **provider plugin** approach; the sidecar fallback is not needed.
- Package name `cfsync` via `basePkg`; module path is lowercase
  `github.com/kahooli/traefik-plugin-cloudflare-ddns`.
- Stdlib-only, hand-written Cloudflare client (as planned).
- Start with polling (`pollInterval`). Traefik's API has no change feed, so polling every
  15–30 s is cheap (one local request) and Cloudflare is only called when something changed.
- Confirm Docker/`defaultRule` discovery with `examples/docker-compose.yml` on a real host.
