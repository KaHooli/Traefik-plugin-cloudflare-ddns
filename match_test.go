package cfsync

import (
	"reflect"
	"testing"
)

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern, host string
		want          bool
	}{
		{"mail.example.com", "mail.example.com", true},
		{"mail.example.com", "MAIL.example.com", true},
		{"mail.example.com", "xmail.example.com", false},
		{"*.example.com", "a.example.com", true},
		{"*.example.com", "a.b.example.com", true},
		{"*.example.com", "example.com", false},
		{"*", "anything.example.com", true},
		{"vpn.*", "vpn.example.net", true},
		{"*.mx.*", "a.mx.example.com", true},
		{"*.mx.*", "a.example.com", false},
		{"a*b*c", "abc", true},
		{"a*b*c", "acb", false},
	}
	for _, tt := range tests {
		if got := globMatch(tt.pattern, tt.host); got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.host, got, tt.want)
		}
	}
}

func TestResolveTargets(t *testing.T) {
	c := testConfig()
	c.EntryPointModes = map[string]string{"tunnel": "tunnel", "lan": "none", "WebSecure": "ddns"}
	c.Exclude = []string{"mail.example.com", "*.legacy.example.com"}
	s := mustSettings(t, c)

	hosts := []discoveredHost{
		{Host: "app.example.com", EntryPoints: []string{"web", "websecure"}},
		{Host: "t.example.com", EntryPoints: []string{"tunnel"}},
		{Host: "nas.example.com", EntryPoints: []string{"lan"}},
		{Host: "mixed.example.com", EntryPoints: []string{"tunnel", "websecure"}},
		{Host: "mail.example.com", EntryPoints: []string{"web"}},
		{Host: "x.legacy.example.com", EntryPoints: []string{"web"}},
		{Host: "noep.example.com"},
	}
	got := resolveTargets(hosts, s)

	want := map[string]string{
		"app.example.com":   modeDDNS,
		"t.example.com":     modeTunnel,
		"nas.example.com":   modeNone,
		"mixed.example.com": modeNone,
		"noep.example.com":  modeDDNS,
	}
	gotModes := make(map[string]string)
	for _, tg := range got {
		gotModes[tg.Host] = tg.Mode
	}
	if !reflect.DeepEqual(gotModes, want) {
		t.Errorf("modes = %v, want %v", gotModes, want)
	}
	for _, tg := range got {
		if (tg.Host == "mixed.example.com") != (tg.Conflict != "") {
			t.Errorf("%s: conflict = %q", tg.Host, tg.Conflict)
		}
	}
}
