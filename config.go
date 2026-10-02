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
	// Tunnel holds settings for hosts published through Cloudflare Tunnel.
	Tunnel TunnelConfig `json:"tunnel,omitempty" yaml:"tunnel,omitempty"`
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
	// IncludeTLSDomains also publishes the routers' tls.domains (main and sans).
	IncludeTLSDomains bool `json:"includeTlsDomains,omitempty" yaml:"includeTlsDomains,omitempty"`
	// IncludeTCPRouters also publishes HostSNI names of TCP routers (DNS-only DDNS).
	IncludeTCPRouters bool `json:"includeTcpRouters,omitempty" yaml:"includeTcpRouters,omitempty"`
}

// CloudflareConfig holds the Cloudflare API settings.
type CloudflareConfig struct {
	// APIToken is a Cloudflare API token with Zone:Read and DNS:Edit.
	APIToken string `json:"apiToken,omitempty" yaml:"apiToken,omitempty"`
	// APITokenFile reads the token from a file (e.g. a Docker secret) instead.
	APITokenFile string `json:"apiTokenFile,omitempty" yaml:"apiTokenFile,omitempty"`
	// Zones is an allow-list of zone names. Empty = every zone the token can see.
	Zones []string `json:"zones,omitempty" yaml:"zones,omitempty"`
	// AccountID is the Cloudflare account that owns the tunnel (tunnel mode only).
	AccountID string `json:"accountId,omitempty" yaml:"accountId,omitempty"`
	// InstanceID marks records created by this Traefik instance. Default: traefik.
	InstanceID string `json:"instanceId,omitempty" yaml:"instanceId,omitempty"`
	// APIURL overrides the Cloudflare API base URL (for testing).
	APIURL string `json:"apiUrl,omitempty" yaml:"apiUrl,omitempty"`
}

// DDNSConfig holds the settings for DDNS records.
type DDNSConfig struct {
	// IPv4 publishes A records. Default: true.
	IPv4 bool `json:"ipv4" yaml:"ipv4"`
	// IPv6 publishes AAAA records. Default: false.
	IPv6 bool `json:"ipv6,omitempty" yaml:"ipv6,omitempty"`
	// IPSources are URLs returning the public IPv4 address as plain text, tried in order.
	IPSources []string `json:"ipSources,omitempty" yaml:"ipSources,omitempty"`
	// IPv6Sources are URLs returning the public IPv6 address as plain text, tried in order.
	IPv6Sources []string `json:"ipv6Sources,omitempty" yaml:"ipv6Sources,omitempty"`
	// StaticIP skips detection and uses this IPv4 address.
	StaticIP string `json:"staticIp,omitempty" yaml:"staticIp,omitempty"`
	// StaticIPv6 skips detection and uses this IPv6 address.
	StaticIPv6 string `json:"staticIpv6,omitempty" yaml:"staticIpv6,omitempty"`
	// IPInterval is how often the public IP is re-detected. Default: 5m.
	IPInterval string `json:"ipInterval,omitempty" yaml:"ipInterval,omitempty"`
	// Proxied sets the Cloudflare proxy (orange cloud) on records. Default: true.
	Proxied bool `json:"proxied" yaml:"proxied"`
	// TTL in seconds for unproxied records; 1 means automatic. Default: 1.
	TTL int `json:"ttl,omitempty" yaml:"ttl,omitempty"`
	// DNSOnlyEntryPoints publishes hosts on these entrypoints without the
	// Cloudflare proxy (grey cloud), e.g. for VPN or non-HTTP services.
	DNSOnlyEntryPoints []string `json:"dnsOnlyEntryPoints,omitempty" yaml:"dnsOnlyEntryPoints,omitempty"`
}

// TunnelConfig holds the settings for tunnel mode.
type TunnelConfig struct {
	// ID is the tunnel UUID. Hosts get a proxied CNAME to <id>.cfargotunnel.com.
	ID string `json:"id,omitempty" yaml:"id,omitempty"`
	// ManageIngress adds and removes the tunnel's public-hostname (ingress)
	// rules through the API. Needs a remotely-managed tunnel. Default: true.
	ManageIngress bool `json:"manageIngress" yaml:"manageIngress"`
	// Service is where cloudflared sends traffic, i.e. Traefik's tunnel
	// entrypoint as reachable from cloudflared (e.g. http://traefik:8081).
	Service string `json:"service,omitempty" yaml:"service,omitempty"`
	// EntryPointServices overrides Service per tunnel entrypoint.
	EntryPointServices map[string]string `json:"entryPointServices,omitempty" yaml:"entryPointServices,omitempty"`
	// OriginServerName sends the public hostname as TLS SNI to an https
	// service, so Traefik picks the right certificate. Default: true.
	OriginServerName bool `json:"originServerName" yaml:"originServerName"`
	// NoTLSVerify disables certificate checks for an https service.
	NoTLSVerify bool `json:"noTLSVerify,omitempty" yaml:"noTLSVerify,omitempty"`
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

var defaultIPv6Sources = []string{
	"https://api6.ipify.org",
	"https://v6.ident.me",
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
			IPv4:       true,
			IPInterval: defaultIPInterval.String(),
			Proxied:    true,
			TTL:        1,
		},
		Tunnel: TunnelConfig{
			ManageIngress:    true,
			OriginServerName: true,
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
	tlsDomains  bool
	tcpRouters  bool

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

	ipv4       bool
	ipv6       bool
	ipSources  []string
	ip6Sources []string
	staticIP   string
	staticIP6  string
	ipInterval time.Duration
	proxied    bool
	ttl        int
	dnsOnlyEPs map[string]bool

	accountID          string
	tunnelID           string
	manageIngress      bool
	tunnelService      string
	entryPointServices map[string]string
	originServerName   bool
	noTLSVerify        bool
}

// tunnelTarget is the CNAME target for tunnel-mode hosts.
func (s *settings) tunnelTarget() string {
	return s.tunnelID + ".cfargotunnel.com"
}

// ingressManaged reports whether tunnel ingress rules are read and written.
// This stays on while a tunnel is configured even if no entrypoint uses
// tunnel mode any more, so rules for pruned CNAMEs are still removed.
func (s *settings) ingressManaged() bool {
	return s.manageIngress && s.tunnelID != "" && s.accountID != ""
}

// usesMode reports whether any entrypoint (or the default) maps to mode.
func (s *settings) usesMode(mode string) bool {
	if s.defaultMode == mode {
		return true
	}
	for _, m := range s.entryPointModes {
		if m == mode {
			return true
		}
	}
	return false
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
		tlsDomains:  c.TraefikAPI.IncludeTLSDomains,
		tcpRouters:  c.TraefikAPI.IncludeTCPRouters,
		ipv4:        c.DDNS.IPv4,
		ipv6:        c.DDNS.IPv6,
		dnsOnlyEPs:  toSet(c.DDNS.DNSOnlyEntryPoints),
		adopt:       c.Adopt,
		prune:       c.Prune,
		zones:       toSet(c.Cloudflare.Zones),
		proxied:     c.DDNS.Proxied,
		ttl:         c.DDNS.TTL,

		accountID:        strings.TrimSpace(c.Cloudflare.AccountID),
		tunnelID:         strings.ToLower(strings.TrimSpace(c.Tunnel.ID)),
		manageIngress:    c.Tunnel.ManageIngress,
		tunnelService:    strings.TrimSpace(c.Tunnel.Service),
		originServerName: c.Tunnel.OriginServerName,
		noTLSVerify:      c.Tunnel.NoTLSVerify,
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
	for _, src := range c.DDNS.IPv6Sources {
		src = strings.TrimSpace(src)
		if src != "" {
			s.ip6Sources = append(s.ip6Sources, src)
		}
	}
	if len(s.ip6Sources) == 0 {
		s.ip6Sources = append([]string(nil), defaultIPv6Sources...)
	}
	if !s.ipv4 && !s.ipv6 && s.usesMode(modeDDNS) {
		return nil, errors.New("ddns.ipv4 and ddns.ipv6 are both off, but an entrypoint uses ddns mode")
	}
	if ip := strings.TrimSpace(c.DDNS.StaticIPv6); ip != "" {
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.To4() != nil {
			return nil, fmt.Errorf("ddns.staticIpv6 must be an IPv6 address, got %q", ip)
		}
		s.staticIP6 = parsed.String()
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

	if err := s.validateTunnel(c.Tunnel.EntryPointServices); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *settings) validateTunnel(epServices map[string]string) error {
	s.entryPointServices = make(map[string]string)
	for ep, svc := range epServices {
		ep = strings.ToLower(strings.TrimSpace(ep))
		svc = strings.TrimSpace(svc)
		if err := validateService(svc); err != nil {
			return fmt.Errorf("tunnel.entryPointServices.%s: %w", ep, err)
		}
		if s.entryPointModes[ep] != modeTunnel && s.defaultMode != modeTunnel {
			return fmt.Errorf("tunnel.entryPointServices.%s: entrypoint is not in tunnel mode", ep)
		}
		s.entryPointServices[ep] = svc
	}

	if s.tunnelID != "" && !isUUID(s.tunnelID) {
		return fmt.Errorf("tunnel.id must be the tunnel UUID, got %q", s.tunnelID)
	}
	if !s.usesMode(modeTunnel) {
		return nil
	}
	if !isUUID(s.tunnelID) {
		return fmt.Errorf("tunnel.id must be the tunnel UUID when an entrypoint uses tunnel mode, got %q", s.tunnelID)
	}
	if !s.manageIngress {
		return nil
	}
	if s.accountID == "" {
		return errors.New("cloudflare.accountId is required to manage tunnel ingress (or set tunnel.manageIngress: false)")
	}
	if s.tunnelService == "" {
		// Every tunnel entrypoint needs a service.
		for ep, m := range s.entryPointModes {
			if m == modeTunnel && s.entryPointServices[ep] == "" {
				return fmt.Errorf("tunnel.service (or tunnel.entryPointServices.%s) is required to manage tunnel ingress", ep)
			}
		}
		if s.defaultMode == modeTunnel {
			return errors.New("tunnel.service is required when defaultMode is tunnel")
		}
		return nil
	}
	return validateService(s.tunnelService)
}

func validateService(svc string) error {
	if !strings.HasPrefix(svc, "http://") && !strings.HasPrefix(svc, "https://") {
		return fmt.Errorf("service must be an http:// or https:// URL, got %q", svc)
	}
	return nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
			if !isHex {
				return false
			}
		}
	}
	return true
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
