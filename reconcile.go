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
	IP         string
	Now        time.Time
	// MissingSince tracks when owned hosts were first seen missing. The
	// planner updates it in place.
	MissingSince map[string]time.Time
}

// plan computes the changes needed to make Cloudflare match the targets.
func plan(in planInput, s *settings) []action {
	var actions []action
	comment := ownerComment(s.instanceID)

	// Hosts whose owned records must not be pruned: DDNS hosts, plus tunnel
	// and conflicting hosts, which later phases (or the user) will resolve.
	keep := make(map[string]bool)

	for _, t := range in.Targets {
		switch {
		case t.Conflict != "":
			keep[t.Host] = true
			actions = append(actions, action{Kind: actSkip, Host: t.Host, Reason: t.Conflict})
			continue
		case t.Mode == modeTunnel:
			keep[t.Host] = true
			actions = append(actions, action{Kind: actSkip, Host: t.Host, Reason: "tunnel mode is not implemented yet"})
			continue
		case t.Mode != modeDDNS:
			continue
		}
		keep[t.Host] = true

		z, ok := zoneFor(t.Host, in.Zones)
		if !ok {
			actions = append(actions, action{Kind: actSkip, Host: t.Host, Reason: "no matching Cloudflare zone"})
			continue
		}
		actions = append(actions, planDDNS(t.Host, z, in.Records[z.ID], in.IP, comment, s)...)
	}

	if s.prune {
		actions = append(actions, planPrune(in, keep, s)...)
	}
	return actions
}

func planDDNS(host string, z zone, records []dnsRecord, ip, comment string, s *settings) []action {
	ttl := s.ttl
	if s.proxied {
		ttl = 1
	}
	desired := dnsRecord{Type: "A", Name: host, Content: ip, Proxied: s.proxied, TTL: ttl, Comment: comment}

	var mine []dnsRecord
	for _, r := range records {
		if !strings.EqualFold(r.Name, host) {
			continue
		}
		switch r.Type {
		case "A", "AAAA", "CNAME":
		default:
			continue // TXT, MX etc. can coexist with an A record
		}
		owned := isOwned(r, s.instanceID)
		if r.Type != "A" {
			who := "foreign"
			if owned {
				who = "managed"
			}
			return []action{{Kind: actSkip, Host: host, Zone: z,
				Reason: fmt.Sprintf("%s %s record exists (-> %s)", who, r.Type, r.Content)}}
		}
		if !owned && !s.adopt {
			return []action{{Kind: actSkip, Host: host, Zone: z,
				Reason: fmt.Sprintf("foreign A record exists (-> %s); set adopt: true to take it over", r.Content)}}
		}
		mine = append(mine, r)
	}

	if len(mine) == 0 {
		return []action{{Kind: actCreate, Host: host, Zone: z, Record: desired}}
	}

	keepIdx := 0
	for i, r := range mine {
		if recordInSync(r, desired, s.instanceID) {
			keepIdx = i
			break
		}
	}

	var actions []action
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

func recordInSync(r, desired dnsRecord, instanceID string) bool {
	if r.Content != desired.Content || r.Proxied != desired.Proxied || !isOwned(r, instanceID) {
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
