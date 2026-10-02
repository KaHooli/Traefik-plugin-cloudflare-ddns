// Package cfsync is a Traefik provider plugin that publishes the hostnames of
// Traefik routers to Cloudflare (DDNS records or Tunnel routes).
package cfsync

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// Config is the plugin configuration, set under providers.plugin.<name> in
// Traefik's static configuration.
type Config struct {
	// PollInterval is how often the Traefik API is read. Default: 30s.
	PollInterval string `json:"pollInterval,omitempty" yaml:"pollInterval,omitempty"`
	// VerifyInterval forces a full Cloudflare check even when nothing changed. Default: 1h.
	VerifyInterval string `json:"verifyInterval,omitempty" yaml:"verifyInterval,omitempty"`
	// DryRun logs the changes that would be made without making them.
	DryRun bool `json:"dryRun,omitempty" yaml:"dryRun,omitempty"`

	// TraefikAPI describes how to reach Traefik's own API.
	TraefikAPI TraefikAPIConfig `json:"traefikApi,omitempty" yaml:"traefikApi,omitempty"`
	// EntryPoints limits discovery to routers on these entrypoints. Empty = all.
	EntryPoints []string `json:"entryPoints,omitempty" yaml:"entryPoints,omitempty"`
	// Providers limits discovery to routers from these providers (docker, file, ...). Empty = all.
	Providers []string `json:"providers,omitempty" yaml:"providers,omitempty"`

	// DefaultMode applies to routers on entrypoints not listed in EntryPointModes:
	// ddns, tunnel or none. Default: ddns.
	DefaultMode string `json:"defaultMode,omitempty" yaml:"defaultMode,omitempty"`
	// EntryPointModes maps an entrypoint name to a mode (ddns, tunnel or none).
	EntryPointModes map[string]string `json:"entryPointModes,omitempty" yaml:"entryPointModes,omitempty"`
	// Exclude lists hostnames (with * wildcards) that are never touched.
	Exclude []string `json:"exclude,omitempty" yaml:"exclude,omitempty"`
	// Adopt lets the plugin take over existing A records it did not create.
	Adopt bool `json:"adopt,omitempty" yaml:"adopt,omitempty"`
	// Prune deletes records this instance created once their hostname is no
	// longer served by Traefik. Default: true.
	Prune bool `json:"prune" yaml:"prune"`
	// PruneGrace is how long a hostname must be gone before its record is deleted. Default: 15m.
	PruneGrace string `json:"pruneGrace,omitempty" yaml:"pruneGrace,omitempty"`

	// Cloudflare holds API credentials and zone settings.
	Cloudflare CloudflareConfig `json:"cloudflare,omitempty" yaml:"cloudflare,omitempty"`
	// DDNS holds settings for records pointing at the public IP.
	DDNS DDNSConfig `json:"ddns,omitempty" yaml:"ddns,omitempty"`
}

// TraefikAPIConfig holds the connection settings for Traefik's API.
type TraefikAPIConfig struct {
	// URL is the API base, including the /api path. Default: http://127.0.0.1:8080/api.
	URL string `json:"url,omitempty" yaml:"url,omitempty"`
	// Username and Password enable basic auth.
	Username string `json:"username,omitempty" yaml:"username,omitempty"`
	Password string `json:"password,omitempty" yaml:"password,omitempty"`
	// InsecureSkipVerify disables TLS verification for an https API URL.
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty" yaml:"insecureSkipVerify,omitempty"`
	// Timeout per request. Default: 10s.
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// CloudflareConfig holds the Cloudflare API settings.
type CloudflareConfig struct {
	// APIToken is a Cloudflare API token with Zone:Read and DNS:Edit.
	APIToken string `json:"apiToken,omitempty" yaml:"apiToken,omitempty"`
	// APITokenFile reads the token from a file (e.g. a Docker secret) instead.
	APITokenFile string `json:"apiTokenFile,omitempty" yaml:"apiTokenFile,omitempty"`
	// Zones is an allow-list of zone names. Empty = every zone the token can see.
	Zones []string `json:"zones,omitempty" yaml:"zones,omitempty"`
	// InstanceID marks records created by this Traefik instance. Default: traefik.
	InstanceID string `json:"instanceId,omitempty" yaml:"instanceId,omitempty"`
	// APIURL overrides the Cloudflare API base URL (for testing).
	APIURL string `json:"apiUrl,omitempty" yaml:"apiUrl,omitempty"`
}

// DDNSConfig holds the settings for DDNS records.
type DDNSConfig struct {
	// IPSources are URLs returning the public IPv4 address as plain text, tried in order.
	IPSources []string `json:"ipSources,omitempty" yaml:"ipSources,omitempty"`
	// StaticIP skips detection and uses this IPv4 address.
	StaticIP string `json:"staticIp,omitempty" yaml:"staticIp,omitempty"`
	// IPInterval is how often the public IP is re-detected. Default: 5m.
	IPInterval string `json:"ipInterval,omitempty" yaml:"ipInterval,omitempty"`
	// Proxied sets the Cloudflare proxy (orange cloud) on records. Default: true.
	Proxied bool `json:"proxied" yaml:"proxied"`
	// TTL in seconds for unproxied records; 1 means automatic. Default: 1.
	TTL int `json:"ttl,omitempty" yaml:"ttl,omitempty"`
}

// Modes a hostname can be published with.
const (
	modeDDNS   = "ddns"
	modeTunnel = "tunnel"
	modeNone   = "none"
)

const (
	defaultAPIURL         = "http://127.0.0.1:8080/api"
	defaultCloudflareURL  = "https://api.cloudflare.com/client/v4"
	defaultInstanceID     = "traefik"
	defaultPollInterval   = 30 * time.Second
	defaultVerifyInterval = time.Hour
	defaultIPInterval     = 5 * time.Minute
	defaultPruneGrace     = 15 * time.Minute
	defaultTimeout        = 10 * time.Second
	minPollInterval       = 5 * time.Second
	minIPInterval         = 30 * time.Second
)

var defaultIPSources = []string{
	"https://api.ipify.org",
	"https://checkip.amazonaws.com",
	"https://ifconfig.me/ip",
}

// CreateConfig returns the default configuration. Called by Traefik.
func CreateConfig() *Config {
	return &Config{
		PollInterval:   defaultPollInterval.String(),
		VerifyInterval: defaultVerifyInterval.String(),
		DefaultMode:    modeDDNS,
		Prune:          true,
		PruneGrace:     defaultPruneGrace.String(),
		TraefikAPI: TraefikAPIConfig{
			URL:     defaultAPIURL,
			Timeout: defaultTimeout.String(),
		},
		Cloudflare: CloudflareConfig{
			InstanceID: defaultInstanceID,
			APIURL:     defaultCloudflareURL,
		},
		DDNS: DDNSConfig{
			IPInterval: defaultIPInterval.String(),
			Proxied:    true,
			TTL:        1,
		},
	}
}

// settings is the validated form of Config.
type settings struct {
	pollInterval   time.Duration
	verifyInterval time.Duration
	dryRun         bool

	apiURL      string
	username    string
	password    string
	insecure    bool
	timeout     time.Duration
	entryPoints map[string]bool
	providers   map[string]bool

	defaultMode     string
	entryPointModes map[string]string
	exclude         []string
	adopt           bool
	prune           bool
	pruneGrace      time.Duration

	cfToken    string
	cfURL      string
	zones      map[string]bool
	instanceID string

	ipSources  []string
	staticIP   string
	ipInterval time.Duration
	proxied    bool
	ttl        int
}

func (c *Config) validate() (*settings, error) {
	if c == nil {
		return nil, errors.New("config is nil")
	}
	s := &settings{
		dryRun:      c.DryRun,
		username:    c.TraefikAPI.Username,
		password:    c.TraefikAPI.Password,
		insecure:    c.TraefikAPI.InsecureSkipVerify,
		entryPoints: toSet(c.EntryPoints),
		providers:   toSet(c.Providers),
		adopt:       c.Adopt,
		prune:       c.Prune,
		zones:       toSet(c.Cloudflare.Zones),
		proxied:     c.DDNS.Proxied,
		ttl:         c.DDNS.TTL,
	}

	var err error
	if s.pollInterval, err = parseDuration(c.PollInterval, defaultPollInterval); err != nil {
		return nil, fmt.Errorf("pollInterval: %w", err)
	}
	if s.pollInterval < minPollInterval {
		return nil, fmt.Errorf("pollInterval must be at least %s", minPollInterval)
	}
	if s.verifyInterval, err = parseDuration(c.VerifyInterval, defaultVerifyInterval); err != nil {
		return nil, fmt.Errorf("verifyInterval: %w", err)
	}
	if s.verifyInterval < s.pollInterval {
		return nil, errors.New("verifyInterval must not be shorter than pollInterval")
	}
	if s.pruneGrace, err = parseDuration(c.PruneGrace, defaultPruneGrace); err != nil {
		return nil, fmt.Errorf("pruneGrace: %w", err)
	}
	if s.pruneGrace < 0 {
		return nil, errors.New("pruneGrace must not be negative")
	}
	if s.timeout, err = parseDuration(c.TraefikAPI.Timeout, defaultTimeout); err != nil {
		return nil, fmt.Errorf("traefikApi.timeout: %w", err)
	}
	if s.ipInterval, err = parseDuration(c.DDNS.IPInterval, defaultIPInterval); err != nil {
		return nil, fmt.Errorf("ddns.ipInterval: %w", err)
	}
	if s.ipInterval < minIPInterval {
		return nil, fmt.Errorf("ddns.ipInterval must be at least %s", minIPInterval)
	}

	if s.apiURL, err = parseBaseURL(c.TraefikAPI.URL, defaultAPIURL); err != nil {
		return nil, fmt.Errorf("traefikApi.url: %w", err)
	}
	if s.cfURL, err = parseBaseURL(c.Cloudflare.APIURL, defaultCloudflareURL); err != nil {
		return nil, fmt.Errorf("cloudflare.apiUrl: %w", err)
	}

	if s.defaultMode, err = parseMode(c.DefaultMode, modeDDNS); err != nil {
		return nil, fmt.Errorf("defaultMode: %w", err)
	}
	s.entryPointModes = make(map[string]string)
	for ep, m := range c.EntryPointModes {
		mode, err := parseMode(m, "")
		if err != nil {
			return nil, fmt.Errorf("entryPointModes.%s: %w", ep, err)
		}
		s.entryPointModes[strings.ToLower(strings.TrimSpace(ep))] = mode
	}

	for _, p := range c.Exclude {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			s.exclude = append(s.exclude, p)
		}
	}

	if s.cfToken, err = loadToken(c.Cloudflare.APIToken, c.Cloudflare.APITokenFile); err != nil {
		return nil, err
	}
	if s.cfToken == "" && s.publishesAnything() {
		return nil, errors.New("cloudflare.apiToken or cloudflare.apiTokenFile is required")
	}
	s.instanceID = strings.TrimSpace(c.Cloudflare.InstanceID)
	if s.instanceID == "" {
		s.instanceID = defaultInstanceID
	}
	if strings.ContainsAny(s.instanceID, " =") {
		return nil, fmt.Errorf("cloudflare.instanceId must not contain spaces or '=': %q", s.instanceID)
	}

	for _, src := range c.DDNS.IPSources {
		src = strings.TrimSpace(src)
		if src != "" {
			s.ipSources = append(s.ipSources, src)
		}
	}
	if len(s.ipSources) == 0 {
		s.ipSources = append([]string(nil), defaultIPSources...)
	}
	if ip := strings.TrimSpace(c.DDNS.StaticIP); ip != "" {
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.To4() == nil {
			return nil, fmt.Errorf("ddns.staticIp must be an IPv4 address, got %q", ip)
		}
		s.staticIP = parsed.String()
	}
	if s.ttl != 1 && (s.ttl < 30 || s.ttl > 86400) {
		return nil, fmt.Errorf("ddns.ttl must be 1 (auto) or 30-86400, got %d", s.ttl)
	}

	return s, nil
}

// publishesAnything reports whether any entrypoint can map to a mode that
// writes to Cloudflare. With everything set to none, the plugin only logs.
func (s *settings) publishesAnything() bool {
	if s.defaultMode != modeNone {
		return true
	}
	for _, m := range s.entryPointModes {
		if m != modeNone {
			return true
		}
	}
	return false
}

func parseDuration(s string, def time.Duration) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	return time.ParseDuration(s)
}

func parseBaseURL(s, def string) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(s), "/")
	if u == "" {
		return def, nil
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "", fmt.Errorf("must start with http:// or https://, got %q", u)
	}
	return u, nil
}

func parseMode(s, def string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(s))
	if m == "" && def != "" {
		return def, nil
	}
	switch m {
	case modeDDNS, modeTunnel, modeNone:
		return m, nil
	}
	return "", fmt.Errorf("unknown mode %q (want ddns, tunnel or none)", s)
}

func loadToken(token, file string) (string, error) {
	token = strings.TrimSpace(token)
	file = strings.TrimSpace(file)
	if token != "" && file != "" {
		return "", errors.New("set only one of cloudflare.apiToken and cloudflare.apiTokenFile")
	}
	if file == "" {
		return token, nil
	}
	b, err := os.ReadFile(file) //nolint:gosec // path is set by the Traefik administrator
	if err != nil {
		return "", fmt.Errorf("cloudflare.apiTokenFile: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func toSet(values []string) map[string]bool {
	out := make(map[string]bool)
	for _, v := range values {
		v = strings.ToLower(strings.TrimSpace(v))
		if v != "" {
			out[v] = true
		}
	}
	return out
}
