package cfsync

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Provider is the Traefik provider plugin. It reads router hostnames from the
// Traefik API and keeps matching DNS records in Cloudflare.
type Provider struct {
	name     string
	settings *settings
	api      *apiClient
	cf       *cloudflareClient
	ipClient *http.Client
	logger   *log.Logger
	now      func() time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup

	lastSummary   string
	ip            string
	ip6           string
	ipCheckedAt   time.Time
	lastState     string
	lastReconcile time.Time
	lastOK        bool
	missingSince  map[string]time.Time
	tunnelChecked bool
}

// New creates the provider. Called by Traefik.
func New(_ context.Context, config *Config, name string) (*Provider, error) {
	s, err := config.validate()
	if err != nil {
		return nil, fmt.Errorf("%s: invalid configuration: %w", name, err)
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	return &Provider{
		name:         name,
		settings:     s,
		api:          newAPIClient(s),
		cf:           newCloudflareClient(s.cfURL, s.cfToken, httpClient),
		ipClient:     &http.Client{Timeout: s.timeout},
		logger:       log.New(os.Stdout, "["+name+"] ", log.LstdFlags),
		now:          time.Now,
		missingSince: make(map[string]time.Time),
	}, nil
}

// Init is called by Traefik before Provide.
func (p *Provider) Init() error {
	s := p.settings
	p.logf("init: api=%s pollInterval=%s defaultMode=%s entryPointModes=%s prune=%t dryRun=%t",
		s.apiURL, s.pollInterval, s.defaultMode, formatModes(s.entryPointModes), s.prune, s.dryRun)
	if s.usesMode(modeTunnel) {
		p.logf("init: tunnel=%s manageIngress=%t service=%s", s.tunnelID, s.manageIngress, s.tunnelService)
	}
	return nil
}

// Provide starts the background loop and returns immediately. The plugin adds
// no routing configuration, so nothing is sent on the channel; Traefik does not
// require a message (verified in the Phase 0 spike).
func (p *Provider) Provide(_ chan<- json.Marshaler) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.run(ctx)
	}()
	return nil
}

// Stop is called by Traefik on shutdown.
func (p *Provider) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	p.logf("stopped")
	return nil
}

func (p *Provider) run(ctx context.Context) {
	// The API is usually not serving yet when Provide is called, so the first
	// poll retries with back-off before settling into the normal interval.
	backoff := time.Second
	for {
		err := p.tick(ctx)
		if err == nil {
			break
		}
		p.logf("waiting for Traefik API (retry in %s): %v", backoff, err)
		if !sleep(ctx, backoff) {
			return
		}
		backoff *= 2
		if backoff > p.settings.pollInterval {
			backoff = p.settings.pollInterval
		}
	}

	ticker := time.NewTicker(p.settings.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.tick(ctx); err != nil {
				p.logf("error: %v", err)
			}
		}
	}
}

// tick runs one poll. It only returns an error when the Traefik API could not
// be read; Cloudflare problems are logged and retried on the next tick.
func (p *Provider) tick(ctx context.Context) error {
	reqCtx, cancel := context.WithTimeout(ctx, p.settings.timeout)
	routers, err := p.api.listRouters(reqCtx, p.settings.tcpRouters)
	cancel()
	if err != nil {
		return err
	}

	hosts, skipped := collectHosts(routers, p.settings)
	targets := resolveTargets(hosts, p.settings)

	if summary := formatSummary(len(routers), targets, skipped); summary != p.lastSummary {
		p.lastSummary = summary
		p.logger.Print(summary)
	}

	// Nothing can change while no hosts are discovered: there is nothing to
	// publish, and pruning is disabled on an empty result (it is more likely a
	// partial API response than every service being gone).
	if !p.settings.publishesAnything() || len(hosts) == 0 {
		return nil
	}

	needIP := false
	for _, t := range targets {
		if t.Mode == modeDDNS && t.Conflict == "" {
			needIP = true
		}
	}
	if needIP {
		p.refreshIP(ctx)
		if p.ip == "" && p.ip6 == "" {
			return nil // nothing to publish DDNS hosts with yet
		}
	}

	now := p.now()
	state := stateKey(p.ip+"/"+p.ip6, targets)
	due := state != p.lastState || !p.lastOK ||
		now.Sub(p.lastReconcile) >= p.settings.verifyInterval ||
		p.pruneDue(now)
	if !due {
		return nil
	}

	ok := p.reconcile(ctx, targets, len(hosts), now)
	p.lastState = state
	p.lastReconcile = now
	p.lastOK = ok
	return nil
}

// refreshIP updates the public addresses of the enabled families. Detection
// errors are logged; the last known address is kept until a new one is found.
func (p *Provider) refreshIP(ctx context.Context) {
	st := p.settings
	if st.ipv4 && st.staticIP != "" {
		p.ip = st.staticIP
	}
	if st.ipv6 && st.staticIP6 != "" {
		p.ip6 = st.staticIP6
	}
	detect4 := st.ipv4 && st.staticIP == ""
	detect6 := st.ipv6 && st.staticIP6 == ""
	if !detect4 && !detect6 {
		return
	}

	now := p.now()
	known := (!detect4 || p.ip != "") && (!detect6 || p.ip6 != "")
	if known && now.Sub(p.ipCheckedAt) < st.ipInterval {
		return
	}
	p.ipCheckedAt = now

	if detect4 {
		ip, err := detectPublicIPv4(ctx, p.ipClient, st.ipSources)
		if err != nil {
			p.logf("error: %v", err)
		} else {
			p.setIP("IPv4", &p.ip, ip)
		}
	}
	if detect6 {
		ip, err := detectPublicIPv6(ctx, p.ipClient, st.ip6Sources)
		if err != nil {
			p.logf("error: %v", err)
		} else {
			p.setIP("IPv6", &p.ip6, ip)
		}
	}
}

func (p *Provider) setIP(family string, current *string, ip string) {
	if ip == *current {
		return
	}
	if *current == "" {
		p.logf("public %s is %s", family, ip)
	} else {
		p.logf("public %s changed: %s -> %s", family, *current, ip)
	}
	*current = ip
}

// pruneDue reports whether a pending deletion's grace period has run out.
func (p *Provider) pruneDue(now time.Time) bool {
	for _, since := range p.missingSince {
		if now.Sub(since) >= p.settings.pruneGrace {
			return true
		}
	}
	return false
}

// reconcile reads Cloudflare, plans and applies changes. It reports whether
// everything succeeded.
func (p *Provider) reconcile(ctx context.Context, targets []target, discovered int, now time.Time) bool {
	allZones, err := p.cf.listZones(ctx)
	if err != nil {
		p.logf("error: list zones: %v", err)
		return false
	}
	zones, missing := filterZones(allZones, p.settings.zones)
	if len(missing) > 0 {
		p.logf("warning: zones not visible to the API token: %s", strings.Join(missing, ", "))
	}

	// Without pruning, only zones containing a published host need reading.
	needed := make(map[string]bool)
	for _, t := range targets {
		if (t.Mode == modeDDNS || t.Mode == modeTunnel) && t.Conflict == "" {
			if z, ok := zoneFor(t.Host, zones); ok {
				needed[z.ID] = true
			}
		}
	}
	records := make(map[string][]dnsRecord)
	for _, z := range zones {
		if !p.settings.prune && !needed[z.ID] {
			continue
		}
		rs, err := p.cf.listRecords(ctx, z.ID)
		if err != nil {
			p.logf("error: list records in %s: %v", z.Name, err)
			return false
		}
		records[z.ID] = rs
	}

	in := planInput{
		Targets:      targets,
		Discovered:   discovered,
		Zones:        zones,
		Records:      records,
		IP:           p.ip,
		IP6:          p.ip6,
		Now:          now,
		MissingSince: p.missingSince,
	}

	var tcfg *tunnelConfig
	if p.settings.ingressManaged() {
		p.checkTunnel(ctx)
		tcfg, err = p.cf.getTunnelConfig(ctx, p.settings.accountID, p.settings.tunnelID)
		if err != nil {
			p.logf("error: read tunnel configuration: %v", err)
			return false
		}
		in.Ingress = tcfg.Ingress
		in.IngressLoaded = true
	}

	planned := plan(in, p.settings)
	actions := planned.Actions

	ingressOK := true
	if planned.Ingress != nil {
		ingressOK = p.applyIngress(ctx, tcfg, planned.Ingress)
		if !ingressOK {
			// Keep the CNAMEs whose rules couldn't be removed, so the next
			// attempt still knows the rules are ours.
			var kept []action
			for _, a := range actions {
				if a.Kind == actDelete && planned.Ingress.Remove[a.Host] {
					continue
				}
				kept = append(kept, a)
			}
			actions = kept
		}
	}

	res := p.apply(ctx, actions)
	if res.Changed > 0 || res.Failed > 0 {
		p.logf("reconcile: %d changed, %d failed, %d skipped, %d pending deletion",
			res.Changed, res.Failed, res.Skipped, res.Pending)
	}
	return res.Failed == 0 && ingressOK
}

// applyIngress merges the planned rules into the tunnel configuration and
// writes it when something changed. It reports whether that succeeded.
func (p *Provider) applyIngress(ctx context.Context, cfg *tunnelConfig, ip *ingressPlan) bool {
	merged, changes := mergeIngress(cfg.Ingress, ip)
	if len(changes) == 0 {
		return true
	}
	prefix := ""
	if p.settings.dryRun {
		prefix = "[dry-run] "
	}
	for _, c := range changes {
		p.logf("%stunnel ingress: %s", prefix, c)
	}
	if p.settings.dryRun {
		return true
	}
	if err := p.cf.putTunnelConfig(ctx, p.settings.accountID, p.settings.tunnelID, cfg, merged); err != nil {
		p.logf("error: write tunnel configuration: %v", err)
		return false
	}
	return true
}

// checkTunnel logs the tunnel's name and status once, and warns when the
// tunnel is locally managed (cloudflared then ignores the remote ingress).
func (p *Provider) checkTunnel(ctx context.Context) {
	if p.tunnelChecked {
		return
	}
	info, err := p.cf.getTunnel(ctx, p.settings.accountID, p.settings.tunnelID)
	if err != nil {
		p.logf("warning: could not read tunnel %s: %v", p.settings.tunnelID, err)
		return
	}
	p.tunnelChecked = true
	p.logf("tunnel %q (%s) status=%s", info.Name, p.settings.tunnelID, info.Status)
	if (info.RemoteConfig != nil && !*info.RemoteConfig) || info.ConfigSrc == "local" {
		p.logf("warning: tunnel %q is locally managed; cloudflared uses its config.yml, so ingress "+
			"rules written by this plugin have no effect. Set tunnel.manageIngress: false and route "+
			"hostnames to Traefik in config.yml instead", info.Name)
	}
}

func (p *Provider) logf(format string, args ...any) {
	p.logger.Printf(format, args...)
}

func stateKey(ip string, targets []target) string {
	var b strings.Builder
	b.WriteString(ip)
	for _, t := range targets {
		b.WriteString("|" + t.Host + "=" + t.Mode + ">" + t.Service + t.Conflict)
		if t.DNSOnly {
			b.WriteString("(dns-only)")
		}
	}
	return b.String()
}

func formatSummary(routerCount int, targets []target, skipped []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "discovered %d host(s) from %d router(s)", len(targets), routerCount)
	for _, t := range targets {
		mode := t.Mode
		if t.Conflict != "" {
			mode = "conflict"
		}
		fmt.Fprintf(&b, "\n  %-6s %s <- %s", mode, t.Host, strings.Join(t.Routers, ", "))
		if t.DNSOnly {
			b.WriteString(" [dns-only]")
		}
	}
	if len(skipped) > 0 {
		fmt.Fprintf(&b, "\n  skipped non-literal matchers in: %s", strings.Join(skipped, ", "))
	}
	return b.String()
}

func formatModes(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	var parts []string
	for ep, mode := range m {
		parts = append(parts, ep+":"+mode)
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, " ") + "}"
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
