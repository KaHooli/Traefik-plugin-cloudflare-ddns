package cfsync

import (
	"strings"
	"testing"
	"time"
)

func TestAdoptFrom(t *testing.T) {
	apex := foreign("CNAME", "t.example.com", "Example.com.") // case and trailing dot ignored
	other := foreign("CNAME", "t.example.com", "elsewhere.example.org")
	fromApex := func(c *Config) { c.AdoptFrom = []string{"example.com"} }

	t.Run("tunnel: CNAME to a listed target is repointed in place", func(t *testing.T) {
		res, got := runTunnelPlan(t, fromApex, planInput{Targets: tunnelTargets("t.example.com"), Records: map[string][]dnsRecord{"z1": {apex}}})
		expect(t, got, "update t.example.com "+testTarget)
		if res.Actions[0].Record.Comment != myComment || res.Actions[0].Record.ID != apex.ID || !res.Actions[0].Record.Proxied {
			t.Errorf("update = %+v", res.Actions[0].Record)
		}
		if wantHosts(res.Ingress) != "want=t.example.com remove=" {
			t.Errorf("ingress %s", wantHosts(res.Ingress))
		}
	})

	t.Run("tunnel: CNAME to an unlisted target stays protected", func(t *testing.T) {
		_, got := runTunnelPlan(t, fromApex, planInput{Targets: tunnelTargets("t.example.com"), Records: map[string][]dnsRecord{"z1": {other}}})
		expect(t, got, "skip t.example.com: foreign CNAME record exists (-> elsewhere.example.org); add elsewhere.example.org to adoptFrom to take it over")
	})

	t.Run("without adoptFrom the hint names the target", func(t *testing.T) {
		_, got := runTunnelPlan(t, nil, planInput{Targets: tunnelTargets("t.example.com"), Records: map[string][]dnsRecord{"z1": {apex}}})
		expect(t, got, "skip t.example.com: foreign CNAME record exists (-> Example.com.); add example.com to adoptFrom to take it over")
	})

	t.Run("ddns: CNAME to a listed target is replaced by an A record", func(t *testing.T) {
		got := runPlan(t, fromApex, planInput{Targets: ddns("t.example.com"), Records: map[string][]dnsRecord{"z1": {apex}}})
		expect(t, got, "delete t.example.com Example.com.", "create t.example.com 203.0.113.10")
	})

	t.Run("adopted records become owned, so they can be pruned later", func(t *testing.T) {
		c := testConfig()
		fromApex(c)
		s := mustSettings(t, c)
		res := plan(planInput{Targets: tunnelTargets("t.example.com"), Zones: []zone{zoneCom}, Now: testNow, Discovered: 1,
			Records: map[string][]dnsRecord{"z1": {apex}}, MissingSince: map[string]time.Time{}, IngressLoaded: true}, s)
		if !isOwned(res.Actions[0].Record, "traefik") {
			t.Errorf("adopted record not marked owned: %q", res.Actions[0].Record.Comment)
		}
	})

	t.Run("adoptFrom alone never touches foreign A records", func(t *testing.T) {
		_, got := runTunnelPlan(t, fromApex, planInput{Targets: tunnelTargets("t.example.com"),
			Records: map[string][]dnsRecord{"z1": {foreign("A", "t.example.com", "203.0.113.10")}}})
		expect(t, got, "skip t.example.com: foreign A record exists (-> 203.0.113.10); set adopt: true to take it over")
	})
}

func TestAdoptAddressRecordForTunnel(t *testing.T) {
	_, got := runTunnelPlan(t, func(c *Config) { c.Adopt = true }, planInput{Targets: tunnelTargets("t.example.com"),
		Records: map[string][]dnsRecord{"z1": {foreign("A", "t.example.com", "203.0.113.10"), foreign("TXT", "t.example.com", "keep")}}})
	// The TXT record still blocks the CNAME: adopt only replaces address records.
	expect(t, got, "skip t.example.com: foreign TXT record exists (-> keep)")

	_, got = runTunnelPlan(t, func(c *Config) { c.Adopt = true }, planInput{Targets: tunnelTargets("t.example.com"),
		Records: map[string][]dnsRecord{"z1": {foreign("A", "t.example.com", "203.0.113.10")}}})
	expect(t, got, "delete t.example.com 203.0.113.10", "create t.example.com "+testTarget)
}

func TestAdoptFromValidation(t *testing.T) {
	for _, bad := range []string{"localhost", "*.example.com", "https://example.com"} {
		c := testConfig()
		c.AdoptFrom = []string{bad}
		if _, err := c.validate(); err == nil || !strings.Contains(err.Error(), "adoptFrom") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
	c := testConfig()
	c.AdoptFrom = []string{" Example.COM. ", ""}
	s := mustSettings(t, c)
	if !s.adoptFrom["example.com"] || len(s.adoptFrom) != 1 {
		t.Errorf("adoptFrom = %v", s.adoptFrom)
	}
}

// End to end: an existing scheme of CNAMEs to the apex is migrated to the
// tunnel in one pass, excluded hosts and unrelated CNAMEs are left alone.
func TestEndToEndMigrateApexCNAMEsToTunnel(t *testing.T) {
	e := newE2E(t, func(c *Config) {
		c.DefaultMode = modeTunnel
		c.EntryPointModes = nil
		c.AdoptFrom = []string{"example.com"}
		c.Adopt = true
	})
	e.cf.add("z1", foreign("CNAME", "a.example.com", "example.com"))
	e.cf.add("z1", foreign("CNAME", "plex.example.com", "example.com")) // excluded
	e.cf.add("z1", foreign("CNAME", "ext.example.com", "elsewhere.example.org"))
	e.cf.add("z2", foreign("CNAME", "b.example.net", "example.com")) // other zone, same target
	e.cf.add("z1", foreign("A", "c.example.com", "203.0.113.10"))
	e.cf.setIngress(`{"service":"http_status:404"}`)
	e.traefik.set(
		rt("a@docker", "websecure", "Host(`a.example.com`)"),
		rt("b@docker", "websecure", "Host(`b.example.net`)"),
		rt("c@docker", "websecure", "Host(`c.example.com`)"),
		rt("ext@docker", "websecure", "Host(`ext.example.com`)"),
		rt("plex@docker", "websecure", "Host(`plex.example.com`)"),
	)
	e.p.settings.exclude = []string{"plex.example.com"}

	writes := e.tick(0)
	expect(t, writes, "tunnel put",
		"delete c.example.com 203.0.113.10",
		"update a.example.com "+testTarget,
		"update b.example.net "+testTarget,
		"create c.example.com "+testTarget)
	expect(t, e.cf.ingress(),
		"a.example.com=http://traefik:8081",
		"b.example.net=http://traefik:8081",
		"c.example.com=http://traefik:8081",
		"<nil>=http_status:404")
	if !strings.Contains(e.out.String(), "skip ext.example.com: foreign CNAME record exists (-> elsewhere.example.org)") {
		t.Errorf("ext not skipped:\n%s", e.out.String())
	}

	// Steady state: nothing more to do.
	expect(t, e.tick(time.Hour))
}
