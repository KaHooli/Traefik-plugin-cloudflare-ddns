// Package cfsync is a Traefik provider plugin that publishes the hostnames of
// Traefik routers to Cloudflare (DDNS records or Tunnel routes).
package cfsync

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Config is the plugin configuration, set under providers.plugin.<name> in
// Traefik's static configuration.
type Config struct {
	// PollInterval is how often the Traefik API is read. Default: 30s.
	PollInterval string `json:"pollInterval,omitempty" yaml:"pollInterval,omitempty"`
	// TraefikAPI describes how to reach Traefik's own API.
	TraefikAPI TraefikAPIConfig `json:"traefikApi,omitempty" yaml:"traefikApi,omitempty"`
	// EntryPoints limits discovery to routers on these entrypoints. Empty = all.
	EntryPoints []string `json:"entryPoints,omitempty" yaml:"entryPoints,omitempty"`
	// Providers limits discovery to routers from these providers (docker, file, ...). Empty = all.
	Providers []string `json:"providers,omitempty" yaml:"providers,omitempty"`
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

const (
	defaultAPIURL       = "http://127.0.0.1:8080/api"
	defaultPollInterval = 30 * time.Second
	defaultTimeout      = 10 * time.Second
	minPollInterval     = 5 * time.Second
)

// CreateConfig returns the default configuration. Called by Traefik.
func CreateConfig() *Config {
	return &Config{
		PollInterval: defaultPollInterval.String(),
		TraefikAPI: TraefikAPIConfig{
			URL:     defaultAPIURL,
			Timeout: defaultTimeout.String(),
		},
	}
}

// settings is the validated form of Config.
type settings struct {
	pollInterval time.Duration
	apiURL       string
	username     string
	password     string
	insecure     bool
	timeout      time.Duration
	entryPoints  map[string]bool
	providers    map[string]bool
}

func (c *Config) validate() (*settings, error) {
	if c == nil {
		return nil, errors.New("config is nil")
	}

	poll, err := parseDuration(c.PollInterval, defaultPollInterval)
	if err != nil {
		return nil, fmt.Errorf("pollInterval: %w", err)
	}
	if poll < minPollInterval {
		return nil, fmt.Errorf("pollInterval must be at least %s", minPollInterval)
	}

	timeout, err := parseDuration(c.TraefikAPI.Timeout, defaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("traefikApi.timeout: %w", err)
	}

	apiURL := strings.TrimRight(strings.TrimSpace(c.TraefikAPI.URL), "/")
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	if !strings.HasPrefix(apiURL, "http://") && !strings.HasPrefix(apiURL, "https://") {
		return nil, fmt.Errorf("traefikApi.url must start with http:// or https://, got %q", apiURL)
	}

	return &settings{
		pollInterval: poll,
		apiURL:       apiURL,
		username:     c.TraefikAPI.Username,
		password:     c.TraefikAPI.Password,
		insecure:     c.TraefikAPI.InsecureSkipVerify,
		timeout:      timeout,
		entryPoints:  toSet(c.EntryPoints),
		providers:    toSet(c.Providers),
	}, nil
}

func parseDuration(s string, def time.Duration) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	return time.ParseDuration(s)
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
