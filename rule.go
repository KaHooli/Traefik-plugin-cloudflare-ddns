package cfsync

import "strings"

// extractHosts returns the literal hostnames used in Host() matchers of an
// HTTP router rule. See extractMatcherHosts.
func extractHosts(rule string) ([]string, bool) {
	return extractMatcherHosts(rule, "Host")
}

// extractMatcherHosts returns the literal hostnames used in the given matcher
// ("Host" for HTTP routers, "HostSNI" for TCP routers). It handles both v2
// (Host(`a`, `b`)) and v3 (Host(`a`) || Host(`b`)) syntax, and backtick or
// double-quoted arguments. Regexp matchers, other host matchers and wildcard
// hosts are ignored. The second return value reports whether anything was
// skipped, so callers can log it.
//
// Do not use named results here or in other plugin code: under Yaegi they
// keep their value from the previous call (see docs/spike-results.md).
func extractMatcherHosts(rule, matcher string) ([]string, bool) {
	var hosts []string
	skipped := false
	seen := make(map[string]bool)

	for i := 0; i < len(rule); {
		name, end := readIdent(rule, i)
		if name == "" {
			i++
			continue
		}
		i = end
		if i >= len(rule) || rule[i] != '(' {
			continue
		}

		args, next, ok := readArgs(rule, i)
		if !ok {
			// Unbalanced or malformed; stop rather than guess.
			return hosts, true
		}
		i = next

		if name != matcher {
			if isHostMatcher(name) {
				skipped = true // another kind of host matcher, e.g. HostRegexp
			}
			continue
		}
		for _, a := range args {
			h := normalizeHost(a)
			if h == "" {
				skipped = true
				continue
			}
			if !seen[h] {
				seen[h] = true
				hosts = append(hosts, h)
			}
		}
	}
	return hosts, skipped
}

// readIdent reads an identifier starting at i, if the byte before i is not
// part of an identifier (so "MyHost(" is not read as "Host(").
func readIdent(s string, i int) (string, int) {
	if i > 0 && isIdentByte(s[i-1]) {
		return "", i
	}
	j := i
	for j < len(s) && isIdentByte(s[j]) {
		j++
	}
	return s[i:j], j
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// readArgs reads a parenthesised list of quoted string arguments starting at
// the '(' at index i. It returns the arguments and the index after ')'.
func readArgs(s string, i int) ([]string, int, bool) {
	var args []string
	i++ // skip '('
	for i < len(s) {
		switch c := s[i]; c {
		case ' ', '\t', '\n', '\r', ',':
			i++
		case ')':
			return args, i + 1, true
		case '`', '"':
			end := strings.IndexByte(s[i+1:], c)
			if end < 0 {
				return args, len(s), false
			}
			args = append(args, s[i+1:i+1+end])
			i += end + 2
		default:
			return args, i, false
		}
	}
	return args, i, false
}

// normalizeHost lowercases a host and strips a port and trailing dot. It
// returns "" for values that cannot be published as a DNS record.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h, "]") {
		h = h[:i]
	}
	h = strings.TrimSuffix(h, ".")
	if h == "" || strings.ContainsAny(h, "*{}[]()\\ ") || !strings.Contains(h, ".") {
		return ""
	}
	return h
}

func isHostMatcher(name string) bool {
	switch name {
	case "Host", "HostRegexp", "HostSNI", "HostSNIRegexp":
		return true
	}
	return false
}
