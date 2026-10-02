package cfsync

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const ownerTag = "managed-by=cfsync"

// ownerComment is the comment put on every record this instance manages.
func ownerComment(instanceID string) string {
	return ownerTag + " instance=" + instanceID
}

// isOwned reports whether a record was created (or adopted) by this instance.
func isOwned(r dnsRecord, instanceID string) bool {
	hasTag, hasInstance := false, false
	for _, f := range strings.Fields(r.Comment) {
		switch f {
		case ownerTag:
			hasTag = true
		case "instance=" + instanceID:
			hasInstance = true
		}
	}
	return hasTag && hasInstance
}

// Action kinds.
const (
	actCreate  = "create"
	actUpdate  = "update"
	actDelete  = "delete"
	actSkip    = "skip"
	actPending = "pending-delete"
)

type action struct {
	Kind   string
	Host   string
	Zone   zone
	Record dnsRecord // record to create, the new state for update, or the record to delete
	Old    dnsRecord // previous state for update
	Reason string
	Prune  bool // delete of a host that is no longer served
}

// describe renders the action for logs. It is deliberately not String():
// under Yaegi, fmt does not call String() on interpreted types, so callers
// must call describe() explicitly.
func (a action) describe() string {
	switch a.Kind {
	case actCreate:
		return fmt.Sprintf("create %s %s -> %s (proxied=%t)", a.Record.Type, a.Host, a.Record.Content, a.Record.Proxied)
	case actUpdate:
		return fmt.Sprintf("update %s %s: %s (proxied=%t) -> %s (proxied=%t)", a.Record.Type, a.Host,
			a.Old.Content, a.Old.Proxied, a.Record.Content, a.Record.Proxied)
	case actDelete:
		return fmt.Sprintf("delete %s %s -> %s (%s)", a.Record.Type, a.Host, a.Record.Content, a.Reason)
	case actPending:
		return fmt.Sprintf("%s no longer served; %s %s will be deleted %s", a.Host, a.Record.Type, a.Record.Content, a.Reason)
	}
	return fmt.Sprintf("skip %s: %s", a.Host, a.Reason)
}

// planInput is everything the planner needs; planning itself makes no API calls.
type planInput struct {
	Targets []target
	// Discovered is the number of hosts found before exclusion; pruning is
	// disabled when it is zero, to survive an empty or partial API response.
	Discovered int
	Zones      []zone
	Records    map[string][]dnsRecord // by zone ID
	IP         string                 // public IPv4; empty when unknown or disabled
	IP6        string                 // public IPv6; empty when unknown or disabled
	Now        time.Time
	// MissingSince tracks when owned hosts were first seen missing. The
	// planner updates it in place.
	MissingSince map[string]time.Time
	// Ingress is the tunnel's current ingress list; IngressLoaded is false
	// when ingress isn't managed.
	Ingress       []ingressRule
	IngressLoaded bool
}

// planResult is the outcome of planning: DNS actions, and the ingress
// changes when tunnel ingress is managed (nil otherwise).
type planResult struct {
	Actions []action
	Ingress *ingressPlan
}

// plan computes the changes needed to make Cloudflare match the targets.
func plan(in planInput, s *settings) planResult {
	var res planResult
	if in.IngressLoaded {
		res.Ingress = newIngressPlan()
	}
	comment := ownerComment(s.instanceID)

	// Hosts whose owned records must not be pruned.
	keep := make(map[string]bool)

	for _, t := range in.Targets {
		switch {
		case t.Conflict != "":
			keep[t.Host] = true
			res.Actions = append(res.Actions, action{Kind: actSkip, Host: t.Host, Reason: t.Conflict})
			continue
		case t.Mode != modeDDNS && t.Mode != modeTunnel:
			continue
		}
		keep[t.Host] = true

		z, ok := zoneFor(t.Host, in.Zones)
		if !ok {
			res.Actions = append(res.Actions, action{Kind: actSkip, Host: t.Host, Reason: "no matching Cloudflare zone"})
			continue
		}
		records := in.Records[z.ID]

		if t.Mode == modeDDNS {
			res.Actions = append(res.Actions, planDDNS(t, z, records, in.IP, in.IP6, comment, s)...)
			continue
		}

		// Tunnel mode.
		desired := dnsRecord{Type: "CNAME", Name: t.Host, Content: s.tunnelTarget(), Proxied: true, TTL: 1, Comment: comment}
		rule := desiredIngressRule(t.Host, t.Service, s)
		if res.Ingress != nil {
			existing, found := findHostRule(in.Ingress, t.Host)
			if found && !rulesEqual(existing, rule) && !s.adopt && !hasOwned(records, desired, s.instanceID) {
				res.Actions = append(res.Actions, action{Kind: actSkip, Host: t.Host, Zone: z,
					Reason: fmt.Sprintf("tunnel ingress rule exists (-> %s); set adopt: true to take it over", existing.Service)})
				continue
			}
		}
		acts := planRecord(t.Host, z, records, desired, s)
		res.Actions = append(res.Actions, acts...)
		if res.Ingress != nil && !hasSkip(acts) {
			res.Ingress.Want[t.Host] = rule
		}
	}

	if s.prune {
		res.Actions = append(res.Actions, planPrune(in, keep, s)...)
	}

	// An ingress rule is removed only together with its host's tunnel CNAME,
	// so rules for pending, excluded or conflicting hosts stay in place.
	if res.Ingress != nil {
		for _, a := range res.Actions {
			if a.Kind == actDelete && a.Record.Type == "CNAME" &&
				strings.EqualFold(a.Record.Content, s.tunnelTarget()) && !hasWant(res.Ingress, a.Host) {
				res.Ingress.Remove[a.Host] = true
			}
		}
	}
	return res
}

// planDDNS plans the A and/or AAAA record of a DDNS host. A family whose
// address is unknown (detection failed) is left as it is. If either family
// is blocked by a foreign record, the host is skipped as a whole.
func planDDNS(t target, z zone, records []dnsRecord, ip, ip6, comment string, s *settings) []action {
	proxied := s.proxied && !t.DNSOnly
	ttl := s.ttl
	if proxied {
		ttl = 1
	}
	var wanted []dnsRecord
	if s.ipv4 && ip != "" {
		wanted = append(wanted, dnsRecord{Type: "A", Name: t.Host, Content: ip, Proxied: proxied, TTL: ttl, Comment: comment})
	}
	if s.ipv6 && ip6 != "" {
		wanted = append(wanted, dnsRecord{Type: "AAAA", Name: t.Host, Content: ip6, Proxied: proxied, TTL: ttl, Comment: comment})
	}

	var actions []action
	for _, desired := range wanted {
		acts := planRecord(t.Host, z, records, desired, s)
		if hasSkip(acts) {
			return acts
		}
		actions = append(actions, acts...)
	}
	return actions
}

// planRecord plans one host's record. Records of the desired type are kept or
// updated; owned records of a conflicting type (after a mode change) are
// deleted; any foreign record in the way makes the host a skip.
func planRecord(host string, z zone, records []dnsRecord, desired dnsRecord, s *settings) []action {
	var mine []dnsRecord
	var deletes []action

	for _, r := range records {
		if !strings.EqualFold(r.Name, host) {
			continue
		}
		owned := isOwned(r, s.instanceID)

		if r.Type == desired.Type {
			// Address records can be taken over with adopt; a CNAME only if it
			// already points at the tunnel, or at a target listed in adoptFrom.
			canAdopt := desired.Type != "CNAME" || sameTarget(r.Content, desired.Content)
			mayTake := owned || (s.adopt && canAdopt) || adoptsFrom(r, s)
			if !mayTake {
				return []action{{Kind: actSkip, Host: host, Zone: z,
					Reason: fmt.Sprintf("foreign %s record exists (-> %s)%s", r.Type, r.Content, adoptHint(r, desired, s))}}
			}
			mine = append(mine, r)
			continue
		}

		if !conflicts(desired.Type, r.Type, s) {
			continue
		}
		if !owned && !takesOver(r, desired, s) {
			return []action{{Kind: actSkip, Host: host, Zone: z,
				Reason: fmt.Sprintf("foreign %s record exists (-> %s)%s", r.Type, r.Content, adoptHint(r, desired, s))}}
		}
		reason := "replaced by " + desired.Type + " (mode changed)"
		if !owned {
			reason = "replaced by " + desired.Type + " (adopted)"
		}
		deletes = append(deletes, action{Kind: actDelete, Host: host, Zone: z, Record: r, Reason: reason})
	}

	if len(mine) == 0 {
		return append(deletes, action{Kind: actCreate, Host: host, Zone: z, Record: desired})
	}

	keepIdx := 0
	for i, r := range mine {
		if recordInSync(r, desired, s.instanceID) {
			keepIdx = i
			break
		}
	}

	actions := deletes
	for i, r := range mine {
		if i == keepIdx {
			continue
		}
		actions = append(actions, action{Kind: actDelete, Host: host, Zone: z, Record: r, Reason: "duplicate"})
	}
	if kept := mine[keepIdx]; !recordInSync(kept, desired, s.instanceID) {
		next := desired
		next.ID = kept.ID
		actions = append(actions, action{Kind: actUpdate, Host: host, Zone: z, Record: next, Old: kept})
	}
	return actions
}

// adoptsFrom reports whether r is a foreign CNAME whose target is listed in
// adoptFrom, so it may be taken over.
func adoptsFrom(r dnsRecord, s *settings) bool {
	return r.Type == "CNAME" && s.adoptFrom[normalizeTarget(r.Content)]
}

// takesOver reports whether a foreign record of another type may be deleted
// to make way for the desired record: a CNAME listed in adoptFrom, or (with
// adopt) an address record in the way of a tunnel CNAME.
func takesOver(r, desired dnsRecord, s *settings) bool {
	if adoptsFrom(r, s) {
		return true
	}
	return s.adopt && desired.Type == "CNAME" && (r.Type == "A" || r.Type == "AAAA")
}

// adoptHint tells the user how to let the plugin take over a foreign record
// that blocks a host, when there is a way.
func adoptHint(r, desired dnsRecord, s *settings) string {
	switch {
	case r.Type == "CNAME" && !sameTarget(r.Content, desired.Content):
		return "; add " + normalizeTarget(r.Content) + " to adoptFrom to take it over"
	case !s.adopt && (r.Type == desired.Type || (desired.Type == "CNAME" && (r.Type == "A" || r.Type == "AAAA"))):
		return "; set adopt: true to take it over"
	}
	return ""
}

func sameTarget(a, b string) bool {
	return normalizeTarget(a) == normalizeTarget(b)
}

// conflicts reports whether a record of type other is in the way of the
// desired type at the same name. A CNAME can't share a name with anything.
// A and AAAA coexist when both families are managed; when one family is off,
// a record of that family would send clients somewhere else, so an owned one
// is removed and a foreign one blocks the host.
func conflicts(desired, other string, s *settings) bool {
	switch {
	case desired == "CNAME" || other == "CNAME":
		return true
	case desired == "A" && other == "AAAA":
		return !s.ipv6
	case desired == "AAAA" && other == "A":
		return !s.ipv4
	}
	return false
}

func hasOwned(records []dnsRecord, desired dnsRecord, instanceID string) bool {
	for _, r := range records {
		if strings.EqualFold(r.Name, desired.Name) && r.Type == desired.Type &&
			strings.EqualFold(r.Content, desired.Content) && isOwned(r, instanceID) {
			return true
		}
	}
	return false
}

func hasSkip(actions []action) bool {
	for _, a := range actions {
		if a.Kind == actSkip {
			return true
		}
	}
	return false
}

func hasWant(p *ingressPlan, host string) bool {
	_, ok := p.Want[host]
	return ok
}

func recordInSync(r, desired dnsRecord, instanceID string) bool {
	if !strings.EqualFold(r.Content, desired.Content) || r.Proxied != desired.Proxied || !isOwned(r, instanceID) {
		return false
	}
	// Cloudflare ignores TTL on proxied records.
	return desired.Proxied || r.TTL == desired.TTL
}

func planPrune(in planInput, keep map[string]bool, s *settings) []action {
	if in.Discovered == 0 {
		return nil
	}

	var actions []action
	seen := make(map[string]bool)
	for _, z := range in.Zones {
		for _, r := range in.Records[z.ID] {
			if !isOwned(r, s.instanceID) {
				continue
			}
			host := strings.ToLower(r.Name)
			seen[host] = true
			if keep[host] || matchesAny(s.exclude, host) {
				delete(in.MissingSince, host)
				continue
			}

			since, ok := in.MissingSince[host]
			if !ok {
				since = in.Now
				in.MissingSince[host] = since
			}
			if wait := since.Add(s.pruneGrace).Sub(in.Now); wait > 0 {
				actions = append(actions, action{Kind: actPending, Host: host, Zone: z, Record: r,
					Reason: "in " + wait.Round(time.Second).String()})
				continue
			}
			actions = append(actions, action{Kind: actDelete, Host: host, Zone: z, Record: r, Reason: "no longer served by Traefik", Prune: true})
		}
	}
	for host := range in.MissingSince {
		if !seen[host] {
			delete(in.MissingSince, host)
		}
	}
	return actions
}

// zoneFor returns the zone with the longest name that host is in.
func zoneFor(host string, zones []zone) (zone, bool) {
	best := -1
	for i, z := range zones {
		name := strings.ToLower(z.Name)
		if host != name && !strings.HasSuffix(host, "."+name) {
			continue
		}
		if best < 0 || len(name) > len(zones[best].Name) {
			best = i
		}
	}
	if best < 0 {
		return zone{}, false
	}
	return zones[best], true
}

// filterZones applies the allow-list. It returns the allowed zones and the
// allow-list entries no visible zone matched.
func filterZones(zones []zone, allow map[string]bool) ([]zone, []string) {
	if len(allow) == 0 {
		return zones, nil
	}
	var out []zone
	found := make(map[string]bool)
	for _, z := range zones {
		name := strings.ToLower(z.Name)
		if allow[name] {
			out = append(out, z)
			found[name] = true
		}
	}
	var missing []string
	for name := range allow {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return out, missing
}

// applyResult counts what a reconcile did.
type applyResult struct {
	Changed int
	Skipped int
	Pending int
	Failed  int
}

// apply carries out the actions. Deletes run first, so a duplicate is gone
// before a record with the same content is written.
func (p *Provider) apply(ctx context.Context, actions []action) applyResult {
	var res applyResult
	ordered := make([]action, 0, len(actions))
	for _, a := range actions {
		if a.Kind == actDelete {
			ordered = append(ordered, a)
		}
	}
	for _, a := range actions {
		if a.Kind != actDelete {
			ordered = append(ordered, a)
		}
	}

	prefix := ""
	if p.settings.dryRun {
		prefix = "[dry-run] "
	}
	for _, a := range ordered {
		var err error
		switch a.Kind {
		case actSkip:
			res.Skipped++
			p.logf("%s%s", prefix, a.describe())
			continue
		case actPending:
			res.Pending++
			p.logf("%s%s", prefix, a.describe())
			continue
		case actCreate, actUpdate, actDelete:
		default:
			continue
		}

		p.logf("%s%s [zone %s]", prefix, a.describe(), a.Zone.Name)
		if p.settings.dryRun {
			res.Changed++
			p.prunedHost(a)
			continue
		}
		switch a.Kind {
		case actCreate:
			err = p.cf.createRecord(ctx, a.Zone.ID, a.Record)
		case actUpdate:
			err = p.cf.updateRecord(ctx, a.Zone.ID, a.Record)
		case actDelete:
			err = p.cf.deleteRecord(ctx, a.Zone.ID, a.Record.ID)
		}
		if err != nil {
			res.Failed++
			p.logf("error: %s: %v", a.Host, err)
			continue
		}
		res.Changed++
		p.prunedHost(a)
	}
	return res
}

// prunedHost stops tracking a host once its deletion is done, so the next
// poll doesn't reconcile again just because its grace period has passed. In
// dry-run the host starts a fresh grace period instead of being re-logged on
// every poll.
func (p *Provider) prunedHost(a action) {
	if a.Prune {
		delete(p.missingSince, a.Host)
	}
}
