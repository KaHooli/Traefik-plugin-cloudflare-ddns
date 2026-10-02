package cfsync

import (
	"strings"
	"testing"
	"time"
)

var (
	zoneCom   = zone{ID: "z1", Name: "example.com"}
	zoneNet   = zone{ID: "z2", Name: "example.net"}
	zoneSub   = zone{ID: "z3", Name: "sub.example.com"}
	testNow   = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	myComment = ownerComment("traefik")
)

func ddns(hosts ...string) []target {
	var out []target
	for _, h := range hosts {
		out = append(out, target{Host: h, Mode: modeDDNS})
	}
	return out
}

func owned(name, content string) dnsRecord {
	return dnsRecord{ID: "id-" + name + "-" + content, Type: "A", Name: name, Content: content, Proxied: true, TTL: 1, Comment: myComment}
}

func foreign(typ, name, content string) dnsRecord {
	return dnsRecord{ID: "id-" + name, Type: typ, Name: name, Content: content, TTL: 1}
}

// summarize renders actions as "kind host [content]" strings for comparison.
func summarize(actions []action) []string {
	var out []string
	for _, a := range actions {
		s := a.Kind + " " + a.Host
		switch a.Kind {
		case actCreate, actUpdate, actDelete, actPending:
			s += " " + a.Record.Content
		case actSkip:
			s += ": " + a.Reason
		}
		out = append(out, s)
	}
	return out
}

func runPlan(t *testing.T, mutate func(*Config), in planInput) []string {
	t.Helper()
	c := testConfig()
	if mutate != nil {
		mutate(c)
	}
	s := mustSettings(t, c)
	if in.Zones == nil {
		in.Zones = []zone{zoneCom, zoneNet, zoneSub}
	}
	if in.IP == "" {
		in.IP = "203.0.113.10"
	}
	if in.Now.IsZero() {
		in.Now = testNow
	}
	if in.MissingSince == nil {
		in.MissingSince = make(map[string]time.Time)
	}
	if in.Discovered == 0 {
		in.Discovered = len(in.Targets)
	}
	return summarize(plan(in, s))
}

func expect(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("actions:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestPlanDDNS(t *testing.T) {
	t.Run("create missing record", func(t *testing.T) {
		got := runPlan(t, nil, planInput{Targets: ddns("app.example.com")})
		expect(t, got, "create app.example.com 203.0.113.10")
	})

	t.Run("in sync is a no-op", func(t *testing.T) {
		got := runPlan(t, nil, planInput{
			Targets: ddns("app.example.com"),
			Records: map[string][]dnsRecord{"z1": {owned("app.example.com", "203.0.113.10")}},
		})
		expect(t, got)
	})

	t.Run("ip change updates", func(t *testing.T) {
		got := runPlan(t, nil, planInput{
			Targets: ddns("app.example.com"),
			Records: map[string][]dnsRecord{"z1": {owned("app.example.com", "198.51.100.1")}},
		})
		expect(t, got, "update app.example.com 203.0.113.10")
	})

	t.Run("proxied change updates", func(t *testing.T) {
		got := runPlan(t, func(c *Config) { c.DDNS.Proxied = false }, planInput{
			Targets: ddns("app.example.com"),
			Records: map[string][]dnsRecord{"z1": {owned("app.example.com", "203.0.113.10")}},
		})
		expect(t, got, "update app.example.com 203.0.113.10")
	})

	t.Run("foreign A record is left alone", func(t *testing.T) {
		got := runPlan(t, nil, planInput{
			Targets: ddns("app.example.com"),
			Records: map[string][]dnsRecord{"z1": {foreign("A", "app.example.com", "192.0.2.7")}},
		})
		expect(t, got, "skip app.example.com: foreign A record exists (-> 192.0.2.7); set adopt: true to take it over")
	})

	t.Run("other instance's record is foreign", func(t *testing.T) {
		r := owned("app.example.com", "192.0.2.7")
		r.Comment = ownerComment("other-host")
		got := runPlan(t, nil, planInput{Targets: ddns("app.example.com"), Records: map[string][]dnsRecord{"z1": {r}}})
		if len(got) != 1 || !strings.HasPrefix(got[0], "skip") {
			t.Errorf("got %v", got)
		}
	})

	t.Run("adopt takes over a foreign A record", func(t *testing.T) {
		c := testConfig()
		c.Adopt = true
		s := mustSettings(t, c)
		actions := plan(planInput{
			Targets: ddns("app.example.com"), Zones: []zone{zoneCom}, IP: "203.0.113.10", Now: testNow,
			Records:      map[string][]dnsRecord{"z1": {foreign("A", "app.example.com", "192.0.2.7")}},
			MissingSince: map[string]time.Time{},
		}, s)
		if len(actions) != 1 || actions[0].Kind != actUpdate || actions[0].Record.Comment != myComment {
			t.Fatalf("got %+v", actions)
		}
	})

	t.Run("CNAME is never replaced, even with adopt", func(t *testing.T) {
		got := runPlan(t, func(c *Config) { c.Adopt = true }, planInput{
			Targets: ddns("app.example.com"),
			Records: map[string][]dnsRecord{"z1": {foreign("CNAME", "app.example.com", "elsewhere.example.org")}},
		})
		expect(t, got, "skip app.example.com: foreign CNAME record exists (-> elsewhere.example.org)")
	})

	t.Run("other record types coexist", func(t *testing.T) {
		got := runPlan(t, nil, planInput{
			Targets: ddns("app.example.com"),
			Records: map[string][]dnsRecord{"z1": {foreign("TXT", "app.example.com", "v=spf1 -all")}},
		})
		expect(t, got, "create app.example.com 203.0.113.10")
	})

	t.Run("duplicate owned records are removed", func(t *testing.T) {
		got := runPlan(t, nil, planInput{
			Targets: ddns("app.example.com"),
			Records: map[string][]dnsRecord{"z1": {
				owned("app.example.com", "198.51.100.1"),
				owned("app.example.com", "203.0.113.10"),
			}},
		})
		expect(t, got, "delete app.example.com 198.51.100.1")
	})

	t.Run("longest zone wins across TLDs", func(t *testing.T) {
		c := testConfig()
		s := mustSettings(t, c)
		actions := plan(planInput{
			Targets: ddns("a.sub.example.com", "b.example.com", "c.example.net", "d.example.org"),
			Zones:   []zone{zoneCom, zoneSub, zoneNet}, IP: "203.0.113.10", Now: testNow,
			Records: map[string][]dnsRecord{}, MissingSince: map[string]time.Time{},
		}, s)
		got := make(map[string]string)
		for _, a := range actions {
			got[a.Host] = a.Kind + ":" + a.Zone.Name
		}
		want := map[string]string{
			"a.sub.example.com": "create:sub.example.com",
			"b.example.com":     "create:example.com",
			"c.example.net":     "create:example.net",
			"d.example.org":     "skip:",
		}
		for h, w := range want {
			if got[h] != w {
				t.Errorf("%s: %s, want %s", h, got[h], w)
			}
		}
	})

	t.Run("tunnel and conflict hosts are skipped", func(t *testing.T) {
		got := runPlan(t, nil, planInput{Targets: []target{
			{Host: "t.example.com", Mode: modeTunnel},
			{Host: "m.example.com", Mode: modeNone, Conflict: "entrypoints map to different modes: ddns, tunnel"},
			{Host: "n.example.com", Mode: modeNone},
		}})
		expect(t, got,
			"skip t.example.com: tunnel mode is not implemented yet",
			"skip m.example.com: entrypoints map to different modes: ddns, tunnel")
	})
}

func TestPlanPrune(t *testing.T) {
	records := map[string][]dnsRecord{
		"z1": {
			owned("app.example.com", "203.0.113.10"),
			owned("gone.example.com", "203.0.113.10"),
			owned("mail.example.com", "203.0.113.10"),         // excluded
			owned("tun.example.com", "203.0.113.10"),          // now tunnel mode
			owned("lan.example.com", "203.0.113.10"),          // now mode none
			foreign("A", "other.example.com", "203.0.113.10"), // not ours
		},
		"z2": {owned("old.example.net", "203.0.113.10")},
	}
	targets := []target{
		{Host: "app.example.com", Mode: modeDDNS},
		{Host: "tun.example.com", Mode: modeTunnel},
		{Host: "lan.example.com", Mode: modeNone},
	}
	exclude := func(c *Config) { c.Exclude = []string{"mail.example.com"} }

	missing := make(map[string]time.Time)
	got := runPlan(t, exclude, planInput{Targets: targets, Records: records, MissingSince: missing})
	expect(t, got,
		"skip tun.example.com: tunnel mode is not implemented yet",
		"pending-delete gone.example.com 203.0.113.10",
		"pending-delete lan.example.com 203.0.113.10",
		"pending-delete old.example.net 203.0.113.10")

	// Still within the grace period.
	got = runPlan(t, exclude, planInput{Targets: targets, Records: records, MissingSince: missing, Now: testNow.Add(14 * time.Minute)})
	if len(got) != 4 || !strings.HasPrefix(got[3], "pending-delete") {
		t.Fatalf("within grace: %v", got)
	}

	// Grace period over.
	got = runPlan(t, exclude, planInput{Targets: targets, Records: records, MissingSince: missing, Now: testNow.Add(15 * time.Minute)})
	expect(t, got,
		"skip tun.example.com: tunnel mode is not implemented yet",
		"delete gone.example.com 203.0.113.10",
		"delete lan.example.com 203.0.113.10",
		"delete old.example.net 203.0.113.10")

	// A host that comes back is no longer pending.
	missing = make(map[string]time.Time)
	runPlan(t, exclude, planInput{Targets: targets, Records: records, MissingSince: missing})
	back := append(targets, target{Host: "gone.example.com", Mode: modeDDNS})
	runPlan(t, exclude, planInput{Targets: back, Records: records, MissingSince: missing})
	if _, ok := missing["gone.example.com"]; ok {
		t.Error("gone.example.com still pending after it came back")
	}

	t.Run("nothing discovered disables pruning", func(t *testing.T) {
		got := runPlan(t, nil, planInput{Targets: nil, Discovered: 0, Records: records})
		expect(t, got)
	})

	t.Run("prune disabled", func(t *testing.T) {
		got := runPlan(t, func(c *Config) { c.Prune = false }, planInput{Targets: targets, Records: records})
		expect(t, got, "skip tun.example.com: tunnel mode is not implemented yet")
	})

	t.Run("zero grace deletes immediately", func(t *testing.T) {
		got := runPlan(t, func(c *Config) { c.PruneGrace = "0s" }, planInput{
			Targets: ddns("app.example.com"), Records: map[string][]dnsRecord{"z1": records["z1"][:2]},
		})
		expect(t, got, "delete gone.example.com 203.0.113.10")
	})
}

func TestFilterZones(t *testing.T) {
	zones, missing := filterZones([]zone{zoneCom, zoneNet}, toSet([]string{"Example.NET", "example.org"}))
	if len(zones) != 1 || zones[0].ID != "z2" {
		t.Errorf("zones = %v", zones)
	}
	if len(missing) != 1 || missing[0] != "example.org" {
		t.Errorf("missing = %v", missing)
	}
	all, _ := filterZones([]zone{zoneCom, zoneNet}, nil)
	if len(all) != 2 {
		t.Errorf("no allow-list: %v", all)
	}
}
