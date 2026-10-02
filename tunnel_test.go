package cfsync

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func rule(t *testing.T, js string) ingressRule {
	t.Helper()
	r, err := parseIngressRule(json.RawMessage(js))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func ruleList(rules []ingressRule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, canonicalRule(r.Raw))
	}
	return out
}

func TestDesiredIngressRule(t *testing.T) {
	c := testConfig()
	s := mustSettings(t, c)
	if got := string(desiredIngressRule("a.example.com", "http://traefik:8081", s).Raw); got != `{"hostname":"a.example.com","service":"http://traefik:8081"}` {
		t.Errorf("http rule = %s", got)
	}
	if got := string(desiredIngressRule("a.example.com", "https://traefik:443", s).Raw); got != `{"hostname":"a.example.com","service":"https://traefik:443","originRequest":{"originServerName":"a.example.com"}}` {
		t.Errorf("https rule = %s", got)
	}
	c.Tunnel.OriginServerName = false
	c.Tunnel.NoTLSVerify = true
	s = mustSettings(t, c)
	if got := string(desiredIngressRule("a.example.com", "https://traefik:443", s).Raw); got != `{"hostname":"a.example.com","service":"https://traefik:443","originRequest":{"noTLSVerify":true}}` {
		t.Errorf("https noTLSVerify rule = %s", got)
	}
}

func TestMergeIngress(t *testing.T) {
	s := mustSettings(t, testConfig())
	want := func(hosts ...string) *ingressPlan {
		p := newIngressPlan()
		for _, h := range hosts {
			p.Want[h] = desiredIngressRule(h, "http://traefik:8081", s)
		}
		return p
	}
	catchAll := `{"service":"http_status:404"}`

	t.Run("empty config", func(t *testing.T) {
		out, changes := mergeIngress(nil, want("b.example.com", "a.example.com"))
		expect(t, ruleList(out),
			`{"hostname":"a.example.com","service":"http://traefik:8081"}`,
			`{"hostname":"b.example.com","service":"http://traefik:8081"}`,
			catchAll)
		expect(t, changes, "add catch-all http_status:404", "add a.example.com -> http://traefik:8081", "add b.example.com -> http://traefik:8081")
	})

	t.Run("nothing to do writes nothing", func(t *testing.T) {
		out, changes := mergeIngress(nil, newIngressPlan())
		if out != nil || changes != nil {
			t.Errorf("out=%v changes=%v", out, changes)
		}
	})

	t.Run("inserted before wildcard, foreign rules and fields kept", func(t *testing.T) {
		existing := []ingressRule{
			rule(t, `{"hostname":"ssh.example.com","service":"ssh://localhost:22","originRequest":{"proxyType":"socks"}}`),
			rule(t, `{"hostname":"*.example.net","service":"http://other:80"}`),
			rule(t, `{"service":"http_status:404","originRequest":{}}`),
		}
		out, changes := mergeIngress(existing, want("a.example.net"))
		if string(out[0].Raw) != string(existing[0].Raw) || string(out[3].Raw) != string(existing[2].Raw) {
			t.Error("foreign rules were rewritten")
		}
		expect(t, ruleList(out),
			`{"hostname":"ssh.example.com","originRequest":{"proxyType":"socks"},"service":"ssh://localhost:22"}`,
			`{"hostname":"a.example.net","service":"http://traefik:8081"}`,
			`{"hostname":"*.example.net","service":"http://other:80"}`,
			catchAll)
		expect(t, changes, "add a.example.net -> http://traefik:8081")
	})

	t.Run("normalised equal rule is unchanged", func(t *testing.T) {
		existing := []ingressRule{
			rule(t, `{"hostname":"A.example.com","service":"http://traefik:8081","originRequest":{},"path":""}`),
			rule(t, catchAll),
		}
		out, changes := mergeIngress(existing, want("a.example.com"))
		if len(changes) != 0 || len(out) != 2 {
			t.Errorf("changes=%v out=%v", changes, ruleList(out))
		}
	})

	t.Run("update in place, remove, keep path rules", func(t *testing.T) {
		existing := []ingressRule{
			rule(t, `{"hostname":"a.example.com","service":"http://old:80"}`),
			rule(t, `{"hostname":"gone.example.com","path":"/api","service":"http://api:80"}`),
			rule(t, `{"hostname":"gone.example.com","service":"http://traefik:8081"}`),
			rule(t, `{"hostname":"keep.example.com","service":"http://traefik:8081"}`),
			rule(t, catchAll),
		}
		p := want("a.example.com")
		p.Remove["gone.example.com"] = true
		out, changes := mergeIngress(existing, p)
		expect(t, ruleList(out),
			`{"hostname":"a.example.com","service":"http://traefik:8081"}`,
			`{"hostname":"gone.example.com","path":"/api","service":"http://api:80"}`,
			`{"hostname":"keep.example.com","service":"http://traefik:8081"}`,
			catchAll)
		expect(t, changes, "update a.example.com -> http://traefik:8081", "remove gone.example.com")
	})

	t.Run("duplicate rules for a wanted host are collapsed", func(t *testing.T) {
		existing := []ingressRule{
			rule(t, `{"hostname":"a.example.com","service":"http://traefik:8081"}`),
			rule(t, `{"hostname":"a.example.com","service":"http://traefik:8081"}`),
			rule(t, catchAll),
		}
		out, changes := mergeIngress(existing, want("a.example.com"))
		expect(t, changes, "remove duplicate a.example.com")
		if len(out) != 2 {
			t.Errorf("out = %v", ruleList(out))
		}
	})
}

func tunnelTargets(hosts ...string) []target {
	var out []target
	for _, h := range hosts {
		out = append(out, target{Host: h, Mode: modeTunnel, Service: "http://traefik:8081"})
	}
	return out
}

func ownedCNAME(name string) dnsRecord {
	return dnsRecord{ID: "id-" + name, Type: "CNAME", Name: name, Content: testTarget, Proxied: true, TTL: 1, Comment: myComment}
}

func runTunnelPlan(t *testing.T, mutate func(*Config), in planInput) (planResult, []string) {
	t.Helper()
	c := testConfig()
	if mutate != nil {
		mutate(c)
	}
	s := mustSettings(t, c)
	in.Zones = []zone{zoneCom, zoneNet}
	in.IP = "203.0.113.10"
	if in.Now.IsZero() {
		in.Now = testNow
	}
	if in.MissingSince == nil {
		in.MissingSince = make(map[string]time.Time)
	}
	if in.Discovered == 0 {
		in.Discovered = len(in.Targets)
	}
	in.IngressLoaded = true
	res := plan(in, s)
	return res, summarize(res.Actions)
}

func wantHosts(p *ingressPlan) string {
	var w, r []string
	for h := range p.Want {
		w = append(w, h)
	}
	for h := range p.Remove {
		r = append(r, h)
	}
	return "want=" + strings.Join(sortedStrings(w), ",") + " remove=" + strings.Join(sortedStrings(r), ",")
}

func sortedStrings(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}

func TestPlanTunnel(t *testing.T) {
	t.Run("new host", func(t *testing.T) {
		res, got := runTunnelPlan(t, nil, planInput{Targets: tunnelTargets("t.example.com")})
		expect(t, got, "create t.example.com "+testTarget)
		if wantHosts(res.Ingress) != "want=t.example.com remove=" {
			t.Errorf("ingress %s", wantHosts(res.Ingress))
		}
	})

	t.Run("in sync", func(t *testing.T) {
		res, got := runTunnelPlan(t, nil, planInput{
			Targets: tunnelTargets("t.example.com"),
			Records: map[string][]dnsRecord{"z1": {ownedCNAME("t.example.com")}},
		})
		expect(t, got)
		if wantHosts(res.Ingress) != "want=t.example.com remove=" {
			t.Errorf("ingress %s", wantHosts(res.Ingress))
		}
	})

	foreignRule := `{"hostname":"t.example.com","service":"http://elsewhere:80"}`
	t.Run("foreign ingress rule blocks the host", func(t *testing.T) {
		res, got := runTunnelPlan(t, nil, planInput{
			Targets: tunnelTargets("t.example.com"),
			Ingress: []ingressRule{rule(t, foreignRule)},
		})
		expect(t, got, "skip t.example.com: tunnel ingress rule exists (-> http://elsewhere:80); set adopt: true to take it over")
		if len(res.Ingress.Want) != 0 {
			t.Errorf("ingress %s", wantHosts(res.Ingress))
		}
	})

	t.Run("adopt replaces a foreign ingress rule", func(t *testing.T) {
		res, got := runTunnelPlan(t, func(c *Config) { c.Adopt = true }, planInput{
			Targets: tunnelTargets("t.example.com"),
			Ingress: []ingressRule{rule(t, foreignRule)},
		})
		expect(t, got, "create t.example.com "+testTarget)
		if wantHosts(res.Ingress) != "want=t.example.com remove=" {
			t.Errorf("ingress %s", wantHosts(res.Ingress))
		}
	})

	t.Run("identical manual rule is fine", func(t *testing.T) {
		_, got := runTunnelPlan(t, nil, planInput{
			Targets: tunnelTargets("t.example.com"),
			Ingress: []ingressRule{rule(t, `{"hostname":"t.example.com","service":"http://traefik:8081","originRequest":{}}`)},
		})
		expect(t, got, "create t.example.com "+testTarget)
	})

	t.Run("foreign CNAME to the tunnel can be adopted", func(t *testing.T) {
		rec := foreign("CNAME", "t.example.com", testTarget)
		_, got := runTunnelPlan(t, nil, planInput{Targets: tunnelTargets("t.example.com"), Records: map[string][]dnsRecord{"z1": {rec}}})
		expect(t, got, "skip t.example.com: foreign CNAME record exists (-> "+testTarget+"); set adopt: true to take it over")
		_, got = runTunnelPlan(t, func(c *Config) { c.Adopt = true }, planInput{Targets: tunnelTargets("t.example.com"), Records: map[string][]dnsRecord{"z1": {rec}}})
		expect(t, got, "update t.example.com "+testTarget)
	})

	t.Run("foreign CNAME elsewhere is not adopted by adopt alone", func(t *testing.T) {
		rec := foreign("CNAME", "t.example.com", "other.example.org")
		_, got := runTunnelPlan(t, func(c *Config) { c.Adopt = true }, planInput{Targets: tunnelTargets("t.example.com"), Records: map[string][]dnsRecord{"z1": {rec}}})
		expect(t, got, "skip t.example.com: foreign CNAME record exists (-> other.example.org); add other.example.org to adoptFrom to take it over")
	})

	t.Run("any foreign record blocks a CNAME", func(t *testing.T) {
		rec := foreign("TXT", "t.example.com", "verification")
		res, got := runTunnelPlan(t, nil, planInput{Targets: tunnelTargets("t.example.com"), Records: map[string][]dnsRecord{"z1": {rec}}})
		expect(t, got, "skip t.example.com: foreign TXT record exists (-> verification)")
		if len(res.Ingress.Want) != 0 {
			t.Error("rule wanted for a skipped host")
		}
	})

	t.Run("ddns to tunnel switch", func(t *testing.T) {
		_, got := runTunnelPlan(t, nil, planInput{
			Targets: tunnelTargets("t.example.com"),
			Records: map[string][]dnsRecord{"z1": {owned("t.example.com", "203.0.113.10")}},
		})
		expect(t, got, "delete t.example.com 203.0.113.10", "create t.example.com "+testTarget)
	})

	t.Run("tunnel to ddns switch removes the rule", func(t *testing.T) {
		res, got := runTunnelPlan(t, nil, planInput{
			Targets: ddns("t.example.com"),
			Records: map[string][]dnsRecord{"z1": {ownedCNAME("t.example.com")}},
		})
		expect(t, got, "delete t.example.com "+testTarget, "create t.example.com 203.0.113.10")
		if wantHosts(res.Ingress) != "want= remove=t.example.com" {
			t.Errorf("ingress %s", wantHosts(res.Ingress))
		}
	})

	t.Run("prune removes the rule only with the CNAME", func(t *testing.T) {
		missing := make(map[string]time.Time)
		in := planInput{
			Targets:      ddns("app.example.com"),
			Records:      map[string][]dnsRecord{"z1": {ownedCNAME("gone.example.com"), ownedCNAME("conf.example.com")}},
			MissingSince: missing,
		}
		in.Targets = append(in.Targets, target{Host: "conf.example.com", Mode: modeNone, Conflict: "entrypoints map to different modes: ddns, tunnel"})
		res, got := runTunnelPlan(t, nil, in)
		expect(t, got,
			"create app.example.com 203.0.113.10",
			"skip conf.example.com: entrypoints map to different modes: ddns, tunnel",
			"pending-delete gone.example.com "+testTarget)
		if wantHosts(res.Ingress) != "want= remove=" {
			t.Errorf("pending: ingress %s", wantHosts(res.Ingress))
		}

		in.Now = testNow.Add(15 * time.Minute)
		res, got = runTunnelPlan(t, nil, in)
		if got[len(got)-1] != "delete gone.example.com "+testTarget {
			t.Errorf("after grace: %v", got)
		}
		if wantHosts(res.Ingress) != "want= remove=gone.example.com" {
			t.Errorf("after grace: ingress %s", wantHosts(res.Ingress))
		}
	})

	t.Run("without ingress management only DNS is planned", func(t *testing.T) {
		c := testConfig()
		c.Tunnel.ManageIngress = false
		s := mustSettings(t, c)
		res := plan(planInput{Targets: tunnelTargets("t.example.com"), Zones: []zone{zoneCom}, Now: testNow,
			MissingSince: map[string]time.Time{}, Discovered: 1}, s)
		if res.Ingress != nil || len(res.Actions) != 1 || res.Actions[0].Kind != actCreate {
			t.Errorf("got %+v", res)
		}
	})
}

func TestTunnelConfigValidation(t *testing.T) {
	tunnelEP := func(c *Config) { c.EntryPointModes = map[string]string{"tunnel": "tunnel"} }
	bad := map[string]func(c *Config){
		"missing id":          func(c *Config) { tunnelEP(c); c.Tunnel.ID = "" },
		"bad id":              func(c *Config) { tunnelEP(c); c.Tunnel.ID = "my-tunnel" },
		"missing account":     func(c *Config) { tunnelEP(c); c.Cloudflare.AccountID = "" },
		"missing service":     func(c *Config) { tunnelEP(c); c.Tunnel.Service = "" },
		"bad service":         func(c *Config) { tunnelEP(c); c.Tunnel.Service = "traefik:8081" },
		"override on ddns ep": func(c *Config) { tunnelEP(c); c.Tunnel.EntryPointServices = map[string]string{"web": "http://x"} },
		"default tunnel":      func(c *Config) { c.DefaultMode = "tunnel"; c.Tunnel.Service = "" },
	}
	for name, mutate := range bad {
		c := testConfig()
		mutate(c)
		if _, err := c.validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}

	good := map[string]func(c *Config){
		"tunnel not used": func(c *Config) { c.Tunnel = TunnelConfig{} },
		"dns only": func(c *Config) {
			tunnelEP(c)
			c.Tunnel.ManageIngress = false
			c.Cloudflare.AccountID = ""
			c.Tunnel.Service = ""
		},
		"per-entrypoint only": func(c *Config) {
			tunnelEP(c)
			c.Tunnel.Service = ""
			c.Tunnel.EntryPointServices = map[string]string{"tunnel": "https://traefik:8443"}
		},
		"uppercase id": func(c *Config) { tunnelEP(c); c.Tunnel.ID = strings.ToUpper(testTunnel) },
	}
	for name, mutate := range good {
		c := testConfig()
		mutate(c)
		if _, err := c.validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestServiceForEntryPoint(t *testing.T) {
	c := testConfig()
	c.EntryPointModes = map[string]string{"tunnel": "tunnel", "tunnel-tls": "tunnel"}
	c.Tunnel.EntryPointServices = map[string]string{"tunnel-tls": "https://traefik:8443"}
	s := mustSettings(t, c)
	got := resolveTargets([]discoveredHost{
		{Host: "a.example.com", EntryPoints: []string{"tunnel"}},
		{Host: "b.example.com", EntryPoints: []string{"tunnel-tls"}},
	}, s)
	if got[0].Service != "http://traefik:8081" || got[1].Service != "https://traefik:8443" {
		t.Errorf("services: %q %q", got[0].Service, got[1].Service)
	}
}
