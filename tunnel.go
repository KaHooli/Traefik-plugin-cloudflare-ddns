package cfsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// ingressRule is one rule of a tunnel's ingress list. Raw keeps the rule as
// Cloudflare returned it, so fields this plugin doesn't know survive a write.
type ingressRule struct {
	Hostname string
	Path     string
	Service  string
	Raw      json.RawMessage
}

type ingressFields struct {
	Hostname string `json:"hostname"`
	Path     string `json:"path"`
	Service  string `json:"service"`
}

type originRequestJSON struct {
	OriginServerName string `json:"originServerName,omitempty"`
	NoTLSVerify      bool   `json:"noTLSVerify,omitempty"`
}

type ingressRuleJSON struct {
	Hostname      string             `json:"hostname"`
	Service       string             `json:"service"`
	OriginRequest *originRequestJSON `json:"originRequest,omitempty"`
}

func parseIngressRule(raw json.RawMessage) (ingressRule, error) {
	var f ingressFields
	if err := json.Unmarshal(raw, &f); err != nil {
		return ingressRule{}, err
	}
	return ingressRule{
		Hostname: strings.ToLower(f.Hostname),
		Path:     f.Path,
		Service:  f.Service,
		Raw:      append(json.RawMessage(nil), raw...),
	}, nil
}

// desiredIngressRule builds the rule this plugin wants for a tunnel host.
func desiredIngressRule(host, service string, s *settings) ingressRule {
	r := ingressRuleJSON{Hostname: host, Service: service}
	if strings.HasPrefix(service, "https://") {
		o := &originRequestJSON{NoTLSVerify: s.noTLSVerify}
		if s.originServerName {
			o.OriginServerName = host
		}
		if o.OriginServerName != "" || o.NoTLSVerify {
			r.OriginRequest = o
		}
	}
	raw, _ := json.Marshal(r)
	return ingressRule{Hostname: host, Service: service, Raw: raw}
}

// isCatchAll reports whether a rule matches every request.
func (r ingressRule) isCatchAll() bool {
	return r.Hostname == "" && r.Path == ""
}

// canonicalRule normalises a rule for comparison: keys sorted, empty
// originRequest and path dropped (Cloudflare adds "originRequest": {}).
func canonicalRule(raw json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return string(raw)
	}
	if o, ok := m["originRequest"].(map[string]any); ok && len(o) == 0 {
		delete(m, "originRequest")
	}
	if m["originRequest"] == nil {
		delete(m, "originRequest")
	}
	if p, ok := m["path"].(string); ok && p == "" {
		delete(m, "path")
	}
	if h, ok := m["hostname"].(string); ok {
		m["hostname"] = strings.ToLower(h)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func rulesEqual(a, b ingressRule) bool {
	return canonicalRule(a.Raw) == canonicalRule(b.Raw)
}

// findHostRule returns the exact-hostname, path-less rule for host.
func findHostRule(rules []ingressRule, host string) (ingressRule, bool) {
	for _, r := range rules {
		if r.Hostname == host && r.Path == "" {
			return r, true
		}
	}
	return ingressRule{}, false
}

// ingressPlan says which hosts' rules to add/update and which to remove.
type ingressPlan struct {
	Want   map[string]ingressRule // host -> rule to publish
	Remove map[string]bool        // hosts whose owned CNAME is being deleted
}

func newIngressPlan() *ingressPlan {
	return &ingressPlan{Want: make(map[string]ingressRule), Remove: make(map[string]bool)}
}

// mergeIngress applies the plan to the existing rules. Rules for other hosts,
// path rules and unknown fields are kept in order. New rules go before the
// first wildcard rule (so it can't shadow them) or the catch-all, which stays
// last. It returns the new list and a description of each change; with no
// changes the existing list is returned as is.
func mergeIngress(existing []ingressRule, p *ingressPlan) ([]ingressRule, []string) {
	var out []ingressRule
	var changes []string
	done := make(map[string]bool)

	for _, r := range existing {
		if r.Hostname == "" || r.Path != "" {
			out = append(out, r)
			continue
		}
		if want, ok := p.Want[r.Hostname]; ok {
			if done[r.Hostname] {
				changes = append(changes, "remove duplicate "+r.Hostname)
				continue
			}
			done[r.Hostname] = true
			if rulesEqual(r, want) {
				out = append(out, r)
			} else {
				out = append(out, want)
				changes = append(changes, fmt.Sprintf("update %s -> %s", r.Hostname, want.Service))
			}
			continue
		}
		if p.Remove[r.Hostname] {
			changes = append(changes, "remove "+r.Hostname)
			continue
		}
		out = append(out, r)
	}

	var hosts []string
	for h := range p.Want {
		if !done[h] {
			hosts = append(hosts, h)
		}
	}
	sort.Strings(hosts)
	if len(hosts) == 0 && len(changes) == 0 {
		return existing, nil
	}

	// The last rule must be a catch-all.
	if len(out) == 0 || !out[len(out)-1].isCatchAll() {
		catchAll := ingressRule{Service: "http_status:404", Raw: json.RawMessage(`{"service":"http_status:404"}`)}
		out = append(out, catchAll)
		changes = append(changes, "add catch-all http_status:404")
	}
	if len(hosts) == 0 {
		return out, changes
	}

	at := len(out) - 1
	for i, r := range out {
		if strings.Contains(r.Hostname, "*") {
			at = i
			break
		}
	}
	var added []ingressRule
	for _, h := range hosts {
		added = append(added, p.Want[h])
		changes = append(changes, fmt.Sprintf("add %s -> %s", h, p.Want[h].Service))
	}
	merged := make([]ingressRule, 0, len(out)+len(added))
	merged = append(merged, out[:at]...)
	merged = append(merged, added...)
	merged = append(merged, out[at:]...)
	return merged, changes
}

// tunnelConfig is a tunnel's remote configuration. Fields holds the whole
// "config" object, so settings other than ingress are written back unchanged.
type tunnelConfig struct {
	Fields  map[string]json.RawMessage
	Ingress []ingressRule
}

type tunnelInfo struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	RemoteConfig *bool  `json:"remote_config"`
	ConfigSrc    string `json:"config_src"`
}

func (c *cloudflareClient) tunnelPath(accountID, tunnelID string) string {
	return "/accounts/" + url.PathEscape(accountID) + "/cfd_tunnel/" + url.PathEscape(tunnelID)
}

func (c *cloudflareClient) getTunnel(ctx context.Context, accountID, tunnelID string) (tunnelInfo, error) {
	var info tunnelInfo
	env, err := c.do(ctx, http.MethodGet, c.tunnelPath(accountID, tunnelID), nil)
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(env.Result, &info); err != nil {
		return info, fmt.Errorf("decode tunnel: %w", err)
	}
	return info, nil
}

func (c *cloudflareClient) getTunnelConfig(ctx context.Context, accountID, tunnelID string) (*tunnelConfig, error) {
	env, err := c.do(ctx, http.MethodGet, c.tunnelPath(accountID, tunnelID)+"/configurations", nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		return nil, fmt.Errorf("decode tunnel configuration: %w", err)
	}

	cfg := &tunnelConfig{Fields: result.Config}
	if cfg.Fields == nil {
		cfg.Fields = make(map[string]json.RawMessage)
	}
	var rawRules []json.RawMessage
	if ing, ok := cfg.Fields["ingress"]; ok && string(ing) != "null" {
		if err := json.Unmarshal(ing, &rawRules); err != nil {
			return nil, fmt.Errorf("decode tunnel ingress: %w", err)
		}
	}
	for _, raw := range rawRules {
		r, err := parseIngressRule(raw)
		if err != nil {
			return nil, fmt.Errorf("decode tunnel ingress rule: %w", err)
		}
		cfg.Ingress = append(cfg.Ingress, r)
	}
	return cfg, nil
}

func (c *cloudflareClient) putTunnelConfig(ctx context.Context, accountID, tunnelID string, cfg *tunnelConfig, ingress []ingressRule) error {
	if len(ingress) == 0 || !ingress[len(ingress)-1].isCatchAll() {
		return errors.New("refusing to write tunnel ingress without a final catch-all rule")
	}
	raws := make([]json.RawMessage, 0, len(ingress))
	for _, r := range ingress {
		raws = append(raws, r.Raw)
	}
	ingJSON, err := json.Marshal(raws)
	if err != nil {
		return err
	}

	fields := make(map[string]json.RawMessage, len(cfg.Fields)+1)
	for k, v := range cfg.Fields {
		fields[k] = v
	}
	fields["ingress"] = ingJSON

	_, err = c.do(ctx, http.MethodPut, c.tunnelPath(accountID, tunnelID)+"/configurations",
		map[string]any{"config": fields})
	return err
}
