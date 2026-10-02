package cfsync

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// router is the subset of Traefik's /api/{http,tcp}/routers item that we use.
type router struct {
	Name        string     `json:"name"`
	Rule        string     `json:"rule"`
	Provider    string     `json:"provider"`
	Status      string     `json:"status"`
	EntryPoints []string   `json:"entryPoints"`
	TLS         *routerTLS `json:"tls"`
	// TCP is set for routers read from /api/tcp/routers.
	TCP bool `json:"-"`
}

type routerTLS struct {
	Domains []tlsDomain `json:"domains"`
}

type tlsDomain struct {
	Main string   `json:"main"`
	SANs []string `json:"sans"`
}

// discoveredHost is a hostname together with the routers that use it and
// the union of those routers' entrypoints.
type discoveredHost struct {
	Host        string
	Routers     []string
	EntryPoints []string
	// TCP is set when a TCP router uses the host; it is then published
	// without the Cloudflare proxy, which only handles HTTP.
	TCP bool
}

const maxPages = 100

type apiClient struct {
	base     string
	username string
	password string
	http     *http.Client
}

func newAPIClient(s *settings) *apiClient {
	transport := &http.Transport{Proxy: nil}
	if s.insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in
	}
	return &apiClient{
		base:     s.apiURL,
		username: s.username,
		password: s.password,
		http:     &http.Client{Timeout: s.timeout, Transport: transport},
	}
}

// listRouters reads HTTP routers, plus TCP routers when tcp is set.
func (c *apiClient) listRouters(ctx context.Context, tcp bool) ([]router, error) {
	routers, err := c.listHTTPRouters(ctx)
	if err != nil || !tcp {
		return routers, err
	}
	tcpRouters, err := c.list(ctx, "/tcp/routers")
	if err != nil {
		return nil, err
	}
	for i := range tcpRouters {
		tcpRouters[i].TCP = true
	}
	return append(routers, tcpRouters...), nil
}

// listHTTPRouters reads all pages of /http/routers.
func (c *apiClient) listHTTPRouters(ctx context.Context) ([]router, error) {
	return c.list(ctx, "/http/routers")
}

// list reads all pages of a router list. Traefik paginates with the
// per_page/page query parameters and an X-Next-Page response header.
func (c *apiClient) list(ctx context.Context, path string) ([]router, error) {
	var all []router
	page := 1
	for n := 0; n < maxPages; n++ {
		q := url.Values{}
		q.Set("per_page", "100")
		q.Set("page", strconv.Itoa(page))

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		if c.username != "" {
			req.SetBasicAuth(c.username, c.password)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var items []router
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		all = append(all, items...)

		next, err := strconv.Atoi(resp.Header.Get("X-Next-Page"))
		if err != nil || next <= page {
			return all, nil
		}
		page = next
	}
	return all, fmt.Errorf("more than %d pages of routers", maxPages)
}

// collectHosts filters routers and returns the hostnames they serve, sorted.
// skippedRouters lists routers whose rule had matchers we cannot publish.
func collectHosts(routers []router, s *settings) ([]discoveredHost, []string) {
	var hosts []discoveredHost
	var skippedRouters []string
	byHost := make(map[string][]string)
	entryPoints := make(map[string]map[string]bool)
	tcp := make(map[string]bool)

	for _, r := range routers {
		if r.Status != "" && r.Status != "enabled" {
			continue
		}
		if len(s.providers) > 0 && !s.providers[strings.ToLower(r.Provider)] {
			continue
		}
		if len(s.entryPoints) > 0 && !anyIn(r.EntryPoints, s.entryPoints) {
			continue
		}
		// Traefik's own routers (dashboard/api) are not user services.
		if r.Provider == "internal" {
			continue
		}

		matcher := "Host"
		if r.TCP {
			matcher = "HostSNI"
		}
		found, skipped := extractMatcherHosts(r.Rule, matcher)
		if s.tlsDomains && r.TLS != nil {
			found = appendTLSDomains(found, r.TLS.Domains)
		}
		if skipped {
			skippedRouters = append(skippedRouters, r.Name)
		}
		for _, h := range found {
			if r.TCP {
				tcp[h] = true
			}
			if !containsString(byHost[h], r.Name) {
				byHost[h] = append(byHost[h], r.Name)
			}
			if entryPoints[h] == nil {
				entryPoints[h] = make(map[string]bool)
			}
			for _, ep := range r.EntryPoints {
				entryPoints[h][ep] = true
			}
		}
	}

	for h, rs := range byHost {
		sort.Strings(rs)
		var eps []string
		for ep := range entryPoints[h] {
			eps = append(eps, ep)
		}
		sort.Strings(eps)
		hosts = append(hosts, discoveredHost{Host: h, Routers: rs, EntryPoints: eps, TCP: tcp[h]})
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Host < hosts[j].Host })
	sort.Strings(skippedRouters)
	return hosts, skippedRouters
}

func anyIn(values []string, set map[string]bool) bool {
	for _, v := range values {
		if set[strings.ToLower(v)] {
			return true
		}
	}
	return false
}

// appendTLSDomains adds the literal names of tls.domains, skipping wildcards
// and names already present.
func appendTLSDomains(hosts []string, domains []tlsDomain) []string {
	for _, d := range domains {
		for _, name := range append([]string{d.Main}, d.SANs...) {
			h := normalizeHost(name)
			if h != "" && !containsString(hosts, h) {
				hosts = append(hosts, h)
			}
		}
	}
	return hosts
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
