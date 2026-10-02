package cfsync

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// Provider is the Traefik provider plugin. In this spike it only discovers
// hostnames from the Traefik API and logs them; nothing is written to Cloudflare.
type Provider struct {
	name     string
	settings *settings
	api      *apiClient
	logger   *log.Logger

	cancel context.CancelFunc
	wg     sync.WaitGroup

	lastSummary string
}

// New creates the provider. Called by Traefik.
func New(_ context.Context, config *Config, name string) (*Provider, error) {
	s, err := config.validate()
	if err != nil {
		return nil, fmt.Errorf("%s: invalid configuration: %w", name, err)
	}
	return &Provider{
		name:     name,
		settings: s,
		api:      newAPIClient(s),
		logger:   log.New(os.Stdout, "["+name+"] ", log.LstdFlags),
	}, nil
}

// Init is called by Traefik before Provide.
func (p *Provider) Init() error {
	p.logger.Printf("init: api=%s pollInterval=%s", p.settings.apiURL, p.settings.pollInterval)
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
	p.logger.Printf("stopped")
	return nil
}

func (p *Provider) run(ctx context.Context) {
	// The API is usually not serving yet when Provide is called, so the first
	// poll retries with back-off before settling into the normal interval.
	backoff := time.Second
	for {
		err := p.poll(ctx)
		if err == nil {
			break
		}
		p.logger.Printf("waiting for Traefik API (retry in %s): %v", backoff, err)
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
			if err := p.poll(ctx); err != nil {
				p.logger.Printf("error: %v", err)
			}
		}
	}
}

func (p *Provider) poll(ctx context.Context) error {
	reqCtx, cancel := context.WithTimeout(ctx, p.settings.timeout)
	defer cancel()

	routers, err := p.api.listHTTPRouters(reqCtx)
	if err != nil {
		return err
	}
	hosts, skipped := collectHosts(routers, p.settings)

	summary := formatSummary(len(routers), hosts, skipped)
	if summary == p.lastSummary {
		return nil
	}
	p.lastSummary = summary
	p.logger.Print(summary)
	return nil
}

func formatSummary(routerCount int, hosts []discoveredHost, skipped []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "discovered %d host(s) from %d router(s)", len(hosts), routerCount)
	for _, h := range hosts {
		fmt.Fprintf(&b, "\n  %s <- %s", h.Host, strings.Join(h.Routers, ", "))
	}
	if len(skipped) > 0 {
		fmt.Fprintf(&b, "\n  skipped non-literal matchers in: %s", strings.Join(skipped, ", "))
	}
	return b.String()
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
