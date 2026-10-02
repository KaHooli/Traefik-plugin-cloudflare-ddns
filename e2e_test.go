package cfsync

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTraefik serves /api/http/routers and /api/tcp/routers from mutable lists.
type fakeTraefik struct {
	mu      sync.Mutex
	routers []router
	tcp     []router
}

func (f *fakeTraefik) setTCP(routers ...router) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tcp = routers
}

func (f *fakeTraefik) set(routers ...router) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routers = routers
}

func (f *fakeTraefik) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/http/routers":
		_ = json.NewEncoder(w).Encode(f.routers)
	case "/api/tcp/routers":
		_ = json.NewEncoder(w).Encode(f.tcp)
	default:
		http.NotFound(w, r)
	}
}

// fakeIP serves a mutable public IP.
type fakeIP struct {
	mu sync.Mutex
	ip string
}

func (f *fakeIP) set(ip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ip = ip
}

func (f *fakeIP) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _ = io.WriteString(w, f.ip)
}

func rt(name, ep, rule string) router {
	return router{Name: name, Provider: "file", Status: "enabled", EntryPoints: []string{ep}, Rule: rule}
}

type e2e struct {
	t       *testing.T
	p       *Provider
	traefik *fakeTraefik
	ip      *fakeIP
	ip6     *fakeIP
	cf      *fakeCloudflare
	now     time.Time
	out     *syncBuffer
}

func newE2E(t *testing.T, mutate func(*Config)) *e2e {
	e := &e2e{t: t, traefik: &fakeTraefik{}, ip: &fakeIP{ip: "203.0.113.10"}, ip6: &fakeIP{ip: "2001:db8::10"}, now: testNow, out: &syncBuffer{}}
	e.cf = newFakeCloudflare(t, zoneCom, zoneNet)
	traefikSrv := httptest.NewServer(e.traefik)
	ipSrv := httptest.NewServer(e.ip)
	ip6Srv := httptest.NewServer(e.ip6)
	t.Cleanup(func() { traefikSrv.Close() })
	t.Cleanup(func() { ipSrv.Close() })
	t.Cleanup(func() { ip6Srv.Close() })

	c := testConfig()
	c.TraefikAPI.URL = traefikSrv.URL + "/api"
	c.Cloudflare.APIURL = e.cf.srv.URL
	c.DDNS.IPSources = []string{ipSrv.URL}
	c.DDNS.IPv6Sources = []string{ip6Srv.URL}
	c.EntryPointModes = map[string]string{"tunnel": "tunnel", "lan": "none"}
	c.Exclude = []string{"mail.example.com"}
	// Most tests change the router list and expect pruning on the next tick; the
	// settling behaviour has its own tests that set this back to the default.
	c.PruneSettlePolls = 1
	if mutate != nil {
		mutate(c)
	}
	p, err := New(context.Background(), c, "test")
	if err != nil {
		t.Fatal(err)
	}
	p.logger = log.New(e.out, "", 0)
	p.now = func() time.Time { return e.now }
	e.p = p
	return e
}

func (e *e2e) tick(advance time.Duration) []string {
	e.t.Helper()
	e.now = e.now.Add(advance)
	before := len(e.cf.writeLog())
	if err := e.p.tick(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	return e.cf.writeLog()[before:]
}

func TestEndToEnd(t *testing.T) {
	e := newE2E(t, nil)
	// Records that already exist: one pointing elsewhere (must survive), one
	// excluded, and a stale one this instance created earlier.
	e.cf.add("z1", foreign("A", "legacy.example.com", "192.0.2.50"))
	e.cf.add("z1", owned("mail.example.com", "203.0.113.10"))
	e.cf.add("z2", owned("stale.example.net", "198.51.100.1"))

	e.traefik.set(
		rt("app@docker", "websecure", "Host(`app.example.com`)"),
		rt("alt@file", "websecure", "Host(`app.example.net`)"),
		rt("legacy@file", "websecure", "Host(`legacy.example.com`)"),
		rt("tun@file", "tunnel", "Host(`tun.example.com`)"),
		rt("nas@file", "lan", "Host(`nas.example.com`)"),
		rt("mail@file", "websecure", "Host(`mail.example.com`)"),
	)

	got := e.tick(0)
	expect(t, got, "tunnel put", "create app.example.com 203.0.113.10", "create app.example.net 203.0.113.10",
		"create tun.example.com "+testTarget)
	if !strings.Contains(e.out.String(), "foreign A record exists (-> 192.0.2.50)") {
		t.Errorf("legacy skip not logged:\n%s", e.out.String())
	}

	// Nothing changed: no Cloudflare writes and no re-read.
	expect(t, e.tick(30*time.Second))

	// The stale record is deleted once the grace period has passed.
	expect(t, e.tick(15*time.Minute), "delete stale.example.net 198.51.100.1")

	// IP change: detected after ipInterval, records updated.
	e.ip.set("203.0.113.99")
	expect(t, e.tick(time.Minute))
	expect(t, e.tick(5*time.Minute), "update app.example.com 203.0.113.99", "update app.example.net 203.0.113.99")

	// A container goes away: pending, then deleted after the grace period.
	e.traefik.set(
		rt("app@docker", "websecure", "Host(`app.example.com`)"),
		rt("legacy@file", "websecure", "Host(`legacy.example.com`)"),
		rt("mail@file", "websecure", "Host(`mail.example.com`)"),
	)
	expect(t, e.tick(30*time.Second))
	if !strings.Contains(e.out.String(), "app.example.net no longer served") {
		t.Errorf("pending deletion not logged:\n%s", e.out.String())
	}
	expect(t, e.tick(15*time.Minute), "tunnel put", "delete tun.example.com "+testTarget, "delete app.example.net 203.0.113.99")
	if len(e.p.missingSince) != 0 {
		t.Errorf("still tracking pruned hosts: %v", e.p.missingSince)
	}

	// Final state: foreign and excluded records untouched.
	var names []string
	for _, r := range append(e.cf.records("z1"), e.cf.records("z2")...) {
		names = append(names, r.Name+"="+r.Content)
	}
	expect(t, names, "app.example.com=203.0.113.99", "legacy.example.com=192.0.2.50", "mail.example.com=203.0.113.10")
}

func TestEndToEndDryRun(t *testing.T) {
	e := newE2E(t, func(c *Config) { c.DryRun = true })
	e.cf.add("z2", owned("stale.example.net", "198.51.100.1"))
	e.traefik.set(rt("app@docker", "websecure", "Host(`app.example.com`)"))

	expect(t, e.tick(0))
	expect(t, e.tick(15*time.Minute))
	if n := strings.Count(e.out.String(), "[dry-run] delete"); n != 1 {
		t.Errorf("dry-run delete logged %d times, want 1", n)
	}
	// The next polls must not reconcile (and re-log) on every tick.
	e.tick(30 * time.Second)
	e.tick(30 * time.Second)
	if n := strings.Count(e.out.String(), "[dry-run] delete"); n != 1 {
		t.Errorf("dry-run delete re-logged: %d times", n)
	}
	log := e.out.String()
	for _, want := range []string{"[dry-run] create A app.example.com -> 203.0.113.10", "[dry-run] delete A stale.example.net"} {
		if !strings.Contains(log, want) {
			t.Errorf("missing %q in log:\n%s", want, log)
		}
	}
}

func TestEndToEndCloudflareErrorRetries(t *testing.T) {
	e := newE2E(t, func(c *Config) { c.Cloudflare.APIToken = "wrong" })
	e.traefik.set(rt("app@docker", "websecure", "Host(`app.example.com`)"))
	e.tick(0)
	if e.p.lastOK {
		t.Fatal("reconcile reported success with a bad token")
	}
	e.p.cf.token = "test-token"
	expect(t, e.tick(30*time.Second), "create app.example.com 203.0.113.10")
}

func TestEndToEndTunnel(t *testing.T) {
	e := newE2E(t, func(c *Config) {
		c.EntryPointModes["tunnel-tls"] = "tunnel"
		c.Tunnel.EntryPointServices = map[string]string{"tunnel-tls": "https://traefik:8443"}
	})
	e.cf.setIngress(
		`{"hostname":"ssh.example.com","service":"ssh://localhost:22"}`,
		`{"hostname":"*.example.net","service":"http://other:80"}`,
		`{"service":"http_status:404"}`,
	)
	e.traefik.set(
		rt("t1@docker", "tunnel", "Host(`t1.example.com`)"),
		rt("t2@file", "tunnel-tls", "Host(`t2.example.net`)"),
		rt("web@docker", "websecure", "Host(`web.example.com`)"),
	)

	expect(t, e.tick(0), "tunnel put",
		"create t1.example.com "+testTarget, "create t2.example.net "+testTarget, "create web.example.com 203.0.113.10")
	expect(t, e.cf.ingress(),
		"ssh.example.com=ssh://localhost:22",
		"t1.example.com=http://traefik:8081",
		`t2.example.net=https://traefik:8443 {"originServerName":"t2.example.net"}`,
		"*.example.net=http://other:80",
		"<nil>=http_status:404")
	if !strings.Contains(e.out.String(), `tunnel "home"`) {
		t.Errorf("tunnel check not logged:\n%s", e.out.String())
	}

	// A full verify must not rewrite anything (Cloudflare adds originRequest: {}).
	expect(t, e.tick(time.Hour))

	// The other top-level settings survive the write.
	e.cf.mu.Lock()
	warp := string(e.cf.tunnelConfig["warp-routing"])
	e.cf.mu.Unlock()
	if warp != `{"enabled":false}` {
		t.Errorf("warp-routing = %s", warp)
	}

	// t1 moves to a DDNS entrypoint: rule removed, CNAME replaced by A.
	e.traefik.set(
		rt("t1@docker", "websecure", "Host(`t1.example.com`)"),
		rt("t2@file", "tunnel-tls", "Host(`t2.example.net`)"),
		rt("web@docker", "websecure", "Host(`web.example.com`)"),
	)
	expect(t, e.tick(30*time.Second), "tunnel put", "delete t1.example.com "+testTarget, "create t1.example.com 203.0.113.10")
	if strings.Contains(strings.Join(e.cf.ingress(), " "), "t1.example.com") {
		t.Errorf("t1 rule not removed: %v", e.cf.ingress())
	}

	// t2 goes away: kept during the grace period, then removed with its CNAME.
	// The first removal attempt fails, so the CNAME must be kept for the retry.
	e.traefik.set(
		rt("t1@docker", "websecure", "Host(`t1.example.com`)"),
		rt("web@docker", "websecure", "Host(`web.example.com`)"),
	)
	expect(t, e.tick(30*time.Second))
	if !strings.Contains(strings.Join(e.cf.ingress(), " "), "t2.example.net") {
		t.Error("t2 rule removed during the grace period")
	}
	e.cf.mu.Lock()
	e.cf.failPut = true
	e.cf.mu.Unlock()
	expect(t, e.tick(15*time.Minute))
	e.cf.mu.Lock()
	e.cf.failPut = false
	e.cf.mu.Unlock()
	expect(t, e.tick(30*time.Second), "tunnel put", "delete t2.example.net "+testTarget)
	expect(t, e.cf.ingress(),
		"ssh.example.com=ssh://localhost:22",
		"*.example.net=http://other:80",
		"<nil>=http_status:404")
}

func TestEndToEndTunnelDryRun(t *testing.T) {
	e := newE2E(t, func(c *Config) { c.DryRun = true })
	e.traefik.set(rt("t1@docker", "tunnel", "Host(`t1.example.com`)"))
	expect(t, e.tick(0))
	for _, want := range []string{
		"[dry-run] tunnel ingress: add t1.example.com -> http://traefik:8081",
		"[dry-run] create CNAME t1.example.com -> " + testTarget,
	} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("missing %q in log:\n%s", want, e.out.String())
		}
	}
}

func TestEndToEndPhase3(t *testing.T) {
	e := newE2E(t, func(c *Config) {
		c.DDNS.IPv6 = true
		c.DDNS.DNSOnlyEntryPoints = []string{"vpn"}
		c.TraefikAPI.IncludeTCPRouters = true
		c.TraefikAPI.IncludeTLSDomains = true
	})
	tlsRouter := rt("site@file", "websecure", "Host(`www.example.com`)")
	tlsRouter.TLS = &routerTLS{Domains: []tlsDomain{{Main: "example.com", SANs: []string{"*.example.com", "alt.example.com"}}}}
	e.traefik.set(tlsRouter, rt("wg@file", "vpn", "Host(`vpn.example.com`)"))
	tcp := rt("db@file", "websecure", "HostSNI(`db.example.net`)")
	wild := rt("any@file", "websecure", "HostSNI(`*`)")
	e.traefik.setTCP(tcp, wild)

	e.tick(0)
	type rec struct {
		content string
		proxied bool
	}
	got := make(map[string]rec)
	for _, z := range []string{"z1", "z2"} {
		for _, r := range e.cf.records(z) {
			got[r.Type+" "+r.Name] = rec{r.Content, r.Proxied}
		}
	}
	want := map[string]rec{
		"A www.example.com":    {"203.0.113.10", true},
		"AAAA www.example.com": {"2001:db8::10", true},
		"A example.com":        {"203.0.113.10", true}, // tls.domains main
		"AAAA example.com":     {"2001:db8::10", true},
		"A alt.example.com":    {"203.0.113.10", true}, // tls.domains san; the wildcard is skipped
		"AAAA alt.example.com": {"2001:db8::10", true},
		"A vpn.example.com":    {"203.0.113.10", false}, // dns-only entrypoint
		"AAAA vpn.example.com": {"2001:db8::10", false},
		"A db.example.net":     {"203.0.113.10", false}, // TCP router: never proxied
		"AAAA db.example.net":  {"2001:db8::10", false},
	}
	if len(got) != len(want) {
		t.Errorf("got %d records, want %d: %v", len(got), len(want), got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %+v, want %+v", k, got[k], w)
		}
	}
	if !strings.Contains(e.out.String(), "vpn.example.com <- wg@file [dns-only]") {
		t.Errorf("dns-only not shown in summary:\n%s", e.out.String())
	}

	// IPv6 detection fails: AAAA records are left alone, IPv4 keeps working.
	e.ip6.set("not-an-ip")
	e.ip.set("203.0.113.20")
	writes := e.tick(5 * time.Minute)
	for _, w := range writes {
		if !strings.HasPrefix(w, "update ") || !strings.HasSuffix(w, "203.0.113.20") {
			t.Errorf("unexpected write while IPv6 is unknown: %s", w)
		}
	}
	if len(writes) != 5 {
		t.Errorf("writes = %v", writes)
	}
}

// A tunnel that is still configured keeps its ingress managed after tunnel
// mode is removed from every entrypoint, so pruned hosts lose their rules.
func TestEndToEndTunnelRulesCleanedAfterModeRemoved(t *testing.T) {
	e := newE2E(t, func(c *Config) { c.EntryPointModes = nil })
	e.cf.add("z1", ownedCNAME("old.example.com"))
	e.cf.setIngress(`{"hostname":"old.example.com","service":"http://traefik:8081"}`, `{"service":"http_status:404"}`)
	e.traefik.set(rt("web@docker", "websecure", "Host(`web.example.com`)"))

	expect(t, e.tick(0), "create web.example.com 203.0.113.10")
	expect(t, e.tick(15*time.Minute), "tunnel put", "delete old.example.com "+testTarget)
	expect(t, e.cf.ingress(), "<nil>=http_status:404")
}

func TestEndToEndNoHostsNoCloudflareCalls(t *testing.T) {
	e := newE2E(t, func(c *Config) { c.Cloudflare.APIToken = "wrong" })
	e.traefik.set()
	e.tick(0)
	e.tick(time.Hour)
	if strings.Contains(e.out.String(), "error") {
		t.Errorf("Cloudflare was called with no hosts discovered:\n%s", e.out.String())
	}
}
