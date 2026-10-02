package cfsync

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestExtractHostSNI(t *testing.T) {
	tests := []struct {
		rule    string
		want    []string
		skipped bool
	}{
		{"HostSNI(`db.example.com`)", []string{"db.example.com"}, false},
		{"HostSNI(`a.example.com`) || HostSNI(`b.example.com`)", []string{"a.example.com", "b.example.com"}, false},
		{"HostSNI(`*`)", nil, true},
		{"HostSNIRegexp(`^.+$`)", nil, true},
		{"ClientIP(`10.0.0.0/8`)", nil, false},
	}
	for _, tt := range tests {
		got, skipped := extractMatcherHosts(tt.rule, "HostSNI")
		if !reflect.DeepEqual(got, tt.want) || skipped != tt.skipped {
			t.Errorf("%s: got %v/%v, want %v/%v", tt.rule, got, skipped, tt.want, tt.skipped)
		}
	}
	// HTTP rules don't publish HostSNI names.
	if got, skipped := extractHosts("HostSNI(`db.example.com`)"); got != nil || !skipped {
		t.Errorf("extractHosts(HostSNI) = %v/%v", got, skipped)
	}
}

func TestCollectHostsTCPAndTLS(t *testing.T) {
	c := testConfig()
	c.TraefikAPI.IncludeTLSDomains = true
	s := mustSettings(t, c)
	routers := []router{
		{Name: "web@file", Status: "enabled", EntryPoints: []string{"websecure"}, Rule: "Host(`a.example.com`)",
			TLS: &routerTLS{Domains: []tlsDomain{{Main: "a.example.com", SANs: []string{"b.example.com", "*.example.com"}}}}},
		{Name: "db@file", Status: "enabled", EntryPoints: []string{"websecure"}, Rule: "HostSNI(`a.example.com`)", TCP: true},
	}
	hosts, _ := collectHosts(routers, s)
	if len(hosts) != 2 || hosts[0].Host != "a.example.com" || hosts[1].Host != "b.example.com" {
		t.Fatalf("hosts = %+v", hosts)
	}
	if !hosts[0].TCP || hosts[1].TCP {
		t.Errorf("TCP flags: %v %v", hosts[0].TCP, hosts[1].TCP)
	}
	if !reflect.DeepEqual(hosts[0].Routers, []string{"db@file", "web@file"}) {
		t.Errorf("routers = %v", hosts[0].Routers)
	}

	// Without the flag, TLS domains are ignored.
	hosts, _ = collectHosts(routers[:1], mustSettings(t, testConfig()))
	if len(hosts) != 1 {
		t.Errorf("without includeTlsDomains: %+v", hosts)
	}
}

func TestResolveDNSOnlyAndTCP(t *testing.T) {
	c := testConfig()
	c.EntryPointModes = map[string]string{"tunnel": "tunnel"}
	c.DDNS.DNSOnlyEntryPoints = []string{"vpn"}
	s := mustSettings(t, c)
	got := resolveTargets([]discoveredHost{
		{Host: "web.example.com", EntryPoints: []string{"websecure"}},
		{Host: "vpn.example.com", EntryPoints: []string{"vpn", "websecure"}},
		{Host: "db.example.com", EntryPoints: []string{"websecure"}, TCP: true},
		{Host: "tdb.example.com", EntryPoints: []string{"tunnel"}, TCP: true},
	}, s)
	summary := make(map[string]string)
	for _, tg := range got {
		summary[tg.Host] = tg.Mode
		if tg.DNSOnly {
			summary[tg.Host] += " dns-only"
		}
		if tg.Conflict != "" {
			summary[tg.Host] += " conflict"
		}
	}
	want := map[string]string{
		"web.example.com": "ddns",
		"vpn.example.com": "ddns dns-only",
		"db.example.com":  "ddns dns-only",
		"tdb.example.com": "none conflict",
	}
	if !reflect.DeepEqual(summary, want) {
		t.Errorf("got %v, want %v", summary, want)
	}
}

func ownedAAAA(name, content string) dnsRecord {
	r := owned(name, content)
	r.Type = "AAAA"
	r.ID += "-6"
	return r
}

func TestPlanDualStack(t *testing.T) {
	v6 := func(c *Config) { c.DDNS.IPv6 = true }
	v6only := func(c *Config) { c.DDNS.IPv6 = true; c.DDNS.IPv4 = false }
	host := "app.example.com"
	run := func(mutate func(*Config), ip6 string, records ...dnsRecord) []string {
		c := testConfig()
		if mutate != nil {
			mutate(c)
		}
		s := mustSettings(t, c)
		ip := ""
		if s.ipv4 {
			ip = "203.0.113.10"
		}
		return summarize(plan(planInput{
			Targets: ddns(host), Zones: []zone{zoneCom}, IP: ip, IP6: ip6, Now: testNow, Discovered: 1,
			Records: map[string][]dnsRecord{"z1": records}, MissingSince: map[string]time.Time{},
		}, s).Actions)
	}

	expect(t, run(v6, "2001:db8::10"), "create "+host+" 203.0.113.10", "create "+host+" 2001:db8::10")
	expect(t, run(v6only, "2001:db8::10"), "create "+host+" 2001:db8::10")
	expect(t, run(v6, "2001:db8::10", owned(host, "203.0.113.10"), ownedAAAA(host, "2001:db8::1")), "update "+host+" 2001:db8::10")

	// IPv6 unknown (detection failed): the AAAA record is left alone.
	expect(t, run(v6, "", owned(host, "203.0.113.10"), ownedAAAA(host, "2001:db8::1")))

	// IPv6 turned off: the owned AAAA goes, a foreign one blocks the host.
	expect(t, run(nil, "", owned(host, "203.0.113.10"), ownedAAAA(host, "2001:db8::1")), "delete "+host+" 2001:db8::1")
	expect(t, run(nil, "", foreign("AAAA", host, "2001:db8::99")), "skip "+host+": foreign AAAA record exists (-> 2001:db8::99)")

	// IPv4 turned off: the owned A goes.
	expect(t, run(v6only, "2001:db8::10", owned(host, "203.0.113.10")), "delete "+host+" 203.0.113.10", "create "+host+" 2001:db8::10")

	// A foreign AAAA blocks the whole host, not just the IPv6 half.
	expect(t, run(v6, "2001:db8::10", foreign("AAAA", host, "2001:db8::99")),
		"skip "+host+": foreign AAAA record exists (-> 2001:db8::99); set adopt: true to take it over")
}

func TestPlanDNSOnly(t *testing.T) {
	c := testConfig()
	c.DDNS.TTL = 300
	s := mustSettings(t, c)
	res := plan(planInput{
		Targets: []target{{Host: "vpn.example.com", Mode: modeDDNS, DNSOnly: true}},
		Zones:   []zone{zoneCom}, IP: "203.0.113.10", Now: testNow, Discovered: 1,
		Records:      map[string][]dnsRecord{"z1": {owned("vpn.example.com", "203.0.113.10")}}, // proxied
		MissingSince: map[string]time.Time{},
	}, s)
	if len(res.Actions) != 1 || res.Actions[0].Kind != actUpdate || res.Actions[0].Record.Proxied || res.Actions[0].Record.TTL != 300 {
		t.Errorf("got %+v", res.Actions)
	}
}

func TestDetectPublicIPv6(t *testing.T) {
	v4 := ipServer(t, http.StatusOK, "203.0.113.10")
	ula := ipServer(t, http.StatusOK, "fd00::1")
	linkLocal := ipServer(t, http.StatusOK, "fe80::1")
	good := ipServer(t, http.StatusOK, "2001:db8::10\n")

	ip, err := detectPublicIPv6(context.Background(), http.DefaultClient, []string{v4, ula, linkLocal, good})
	if err != nil || ip != "2001:db8::10" {
		t.Fatalf("ip=%q err=%v", ip, err)
	}
	_, err = detectPublicIPv6(context.Background(), http.DefaultClient, []string{v4, ula})
	if err == nil || !strings.Contains(err.Error(), "public IPv6 detection failed") || !strings.Contains(err.Error(), "not an IPv6 address") {
		t.Errorf("err = %v", err)
	}
}

func TestPhase3ConfigValidation(t *testing.T) {
	bad := map[string]func(c *Config){
		"no family":         func(c *Config) { c.DDNS.IPv4 = false },
		"static v6 is v4":   func(c *Config) { c.DDNS.IPv6 = true; c.DDNS.StaticIPv6 = "203.0.113.1" },
		"unused bad tunnel": func(c *Config) { c.Tunnel.ID = "not-a-uuid" },
	}
	for name, mutate := range bad {
		c := testConfig()
		mutate(c)
		if _, err := c.validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	c := testConfig()
	c.DDNS.IPv4 = false
	c.DDNS.IPv6 = true
	c.DDNS.StaticIPv6 = "2001:DB8::1"
	s, err := c.validate()
	if err != nil || s.staticIP6 != "2001:db8::1" {
		t.Errorf("static v6: %q %v", s.staticIP6, err)
	}
	c = testConfig()
	c.DDNS.IPv4 = false
	c.DefaultMode = modeNone
	if _, err := c.validate(); err != nil {
		t.Errorf("no family but no ddns mode: %v", err)
	}
}
