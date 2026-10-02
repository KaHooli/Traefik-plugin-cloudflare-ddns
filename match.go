package cfsync

import (
	"sort"
	"strings"
)

// globMatch reports whether host matches pattern, case-insensitively. A '*'
// matches any run of characters, including dots, so "*.example.com" matches
// "a.example.com" and "a.b.example.com" but not "example.com".
func globMatch(pattern, host string) bool {
	pattern = strings.ToLower(pattern)
	host = strings.ToLower(host)

	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == host
	}
	if !strings.HasPrefix(host, parts[0]) {
		return false
	}
	rest := host[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, mid)
		if i < 0 {
			return false
		}
		rest = rest[i+len(mid):]
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}

func matchesAny(patterns []string, host string) bool {
	for _, p := range patterns {
		if globMatch(p, host) {
			return true
		}
	}
	return false
}

// target is a discovered hostname with the mode it should be published with.
type target struct {
	Host    string
	Mode    string
	Routers []string
	// Service is the tunnel ingress service for tunnel-mode hosts.
	Service string
	// Conflict is set when the host's entrypoints map to different modes; the
	// host is then left untouched.
	Conflict string
}

// resolveTargets assigns a mode to each host from the entrypoints its routers
// use. Excluded hosts are dropped entirely.
func resolveTargets(hosts []discoveredHost, s *settings) []target {
	var out []target
	for _, h := range hosts {
		if matchesAny(s.exclude, h.Host) {
			continue
		}

		modes := make(map[string]bool)
		for _, ep := range h.EntryPoints {
			modes[modeForEntryPoint(ep, s)] = true
		}
		if len(h.EntryPoints) == 0 {
			modes[s.defaultMode] = true
		}

		t := target{Host: h.Host, Routers: h.Routers}
		if len(modes) == 1 {
			for m := range modes {
				t.Mode = m
			}
		} else {
			var names []string
			for m := range modes {
				names = append(names, m)
			}
			sort.Strings(names)
			t.Mode = modeNone
			t.Conflict = "entrypoints map to different modes: " + strings.Join(names, ", ")
		}
		if t.Mode == modeTunnel {
			t.Service = serviceFor(h.EntryPoints, s)
		}
		out = append(out, t)
	}
	return out
}

// serviceFor picks the ingress service for a tunnel host: the first of its
// entrypoints with an override, else tunnel.service.
func serviceFor(entryPoints []string, s *settings) string {
	for _, ep := range entryPoints {
		if svc := s.entryPointServices[strings.ToLower(ep)]; svc != "" {
			return svc
		}
	}
	return s.tunnelService
}

func modeForEntryPoint(ep string, s *settings) string {
	if m, ok := s.entryPointModes[strings.ToLower(ep)]; ok {
		return m
	}
	return s.defaultMode
}
