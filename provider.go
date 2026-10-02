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
	ipCheckedAt   time.Time
	lastState     string
	lastReconcile time.Time
	lastOK        bool
	missingSince  map[string]time.Time
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
	routers, err := p.api.listHTTPRouters(reqCtx)
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

	if !p.settings.publishesAnything() {
		return nil
	}

	needIP := false
	for _, t := range targets {
		if t.Mode == modeDDNS && t.Conflict == "" {
			needIP = true
		}
	}
	if needIP {
		if err := p.refreshIP(ctx); err != nil {
			p.logf("error: %v", err)
			if p.ip == "" {
				return nil
			}
		}
	}

	now := p.now()
	state := stateKey(p.ip, targets)
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

func (p *Provider) refreshIP(ctx context.Context) error {
	if p.settings.staticIP != "" {
		p.ip = p.settings.staticIP
		return nil
	}
	now := p.now()
	if p.ip != "" && now.Sub(p.ipCheckedAt) < p.settings.ipInterval {
		return nil
	}
	ip, err := detectPublicIPv4(ctx, p.ipClient, p.settings.ipSources)
	if err != nil {
		return err
	}
	p.ipCheckedAt = now
	if ip != p.ip {
		if p.ip == "" {
			p.logf("public IPv4 is %s", ip)
		} else {
			p.logf("public IPv4 changed: %s -> %s", p.ip, ip)
		}
		p.ip = ip
	}
	return nil
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

	// Without pruning, only zones containing a DDNS host need reading.
	needed := make(map[string]bool)
	for _, t := range targets {
		if t.Mode == modeDDNS && t.Conflict == "" {
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

	actions := plan(planInput{
		Targets:      targets,
		Discovered:   discovered,
		Zones:        zones,
		Records:      records,
		IP:           p.ip,
		Now:          now,
		MissingSince: p.missingSince,
	}, p.settings)

	res := p.apply(ctx, actions)
	if res.Changed > 0 || res.Failed > 0 {
		p.logf("reconcile: %d changed, %d failed, %d skipped, %d pending deletion",
			res.Changed, res.Failed, res.Skipped, res.Pending)
	}
	return res.Failed == 0
}

func (p *Provider) logf(format string, args ...any) {
	p.logger.Printf(format, args...)
}

func stateKey(ip string, targets []target) string {
	var b strings.Builder
	b.WriteString(ip)
	for _, t := range targets {
		b.WriteString("|" + t.Host + "=" + t.Mode + t.Conflict)
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
