package cfsync

import (
	"strings"
	"testing"
	"time"
)

func TestApexCNAMEBesideOtherRecords(t *testing.T) {
	mx := foreign("MX", "example.com", "mail.example.org")
	txt := foreign("TXT", "example.com", "v=spf1 -all")

	t.Run("tunnel CNAME at the apex ignores MX and TXT", func(t *testing.T) {
		_, got := runTunnelPlan(t, nil, planInput{Targets: tunnelTargets("example.com"),
			Records: map[string][]dnsRecord{"z1": {mx, txt}}})
		expect(t, got, "create example.com "+testTarget)
	})

	t.Run("a foreign A record at the apex still blocks", func(t *testing.T) {
		_, got := runTunnelPlan(t, nil, planInput{Targets: tunnelTargets("example.com"),
			Records: map[string][]dnsRecord{"z1": {mx, foreign("A", "example.com", "192.0.2.1")}}})
		if len(got) != 1 || !strings.HasPrefix(got[0], "skip example.com: foreign A record exists") {
			t.Errorf("got %q", got)
		}
	})

	t.Run("below the apex an MX still blocks a CNAME", func(t *testing.T) {
		_, got := runTunnelPlan(t, nil, planInput{Targets: tunnelTargets("t.example.com"),
			Records: map[string][]dnsRecord{"z1": {foreign("MX", "t.example.com", "mail.example.org")}}})
		if len(got) != 1 || !strings.HasPrefix(got[0], "skip t.example.com: foreign MX record exists") {
			t.Errorf("got %q", got)
		}
	})
}

func TestPruneWaitsForSettledRouterList(t *testing.T) {
	e := newE2E(t, func(c *Config) { c.PruneSettlePolls = 0 }) // default: 3
	e.cf.add("z1", owned("a.example.com", "203.0.113.10"))
	e.cf.add("z1", owned("b.example.com", "203.0.113.10"))
	all := []router{
		rt("a@docker", "websecure", "Host(`a.example.com`)"),
		rt("b@docker", "websecure", "Host(`b.example.com`)"),
	}

	// Traefik has only loaded some routers yet: nothing is scheduled for deletion.
	e.traefik.set(all[0])
	expect(t, e.tick(0))
	e.traefik.set(all...)
	for i := 0; i < 4; i++ {
		expect(t, e.tick(30*time.Second))
	}
	if strings.Contains(e.out.String(), "no longer served") || len(e.p.missingSince) != 0 {
		t.Fatalf("partial list scheduled a deletion:\n%s", e.out.String())
	}

	// A real removal starts its grace period once the new list has settled.
	e.traefik.set(all[0])
	expect(t, e.tick(30*time.Second))
	expect(t, e.tick(30*time.Second))
	if len(e.p.missingSince) != 0 {
		t.Fatalf("grace period started before the list settled: %v", e.p.missingSince)
	}
	expect(t, e.tick(30*time.Second))
	if !strings.Contains(e.out.String(), "b.example.com no longer served") {
		t.Fatalf("pending deletion not logged:\n%s", e.out.String())
	}
	expect(t, e.tick(15*time.Minute), "delete b.example.com 203.0.113.10")
}

func TestPruneSettlePollsValidation(t *testing.T) {
	c := testConfig()
	c.PruneSettlePolls = -1
	if _, err := c.validate(); err == nil || !strings.Contains(err.Error(), "pruneSettlePolls") {
		t.Errorf("err = %v", err)
	}
}
