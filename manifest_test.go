package cfsync

import (
	"context"
	"testing"
)

// TestManifestTestData mirrors .traefik.yml's testData, which the plugin
// catalog uses to instantiate the plugin; it must pass validation.
func TestManifestTestData(t *testing.T) {
	c := CreateConfig()
	c.DryRun = true
	c.PollInterval = "30s"
	c.TraefikAPI.URL = "http://127.0.0.1:8080/api"
	c.EntryPointModes = map[string]string{"tunnel": "tunnel"}
	c.Exclude = []string{"mail.example.com"}
	c.Cloudflare.APIToken = "CHANGE_ME"
	c.Cloudflare.AccountID = "0123456789abcdef0123456789abcdef"
	c.Tunnel.ID = "6ff42ae2-765d-4adf-8112-31c55c1551ef"
	c.Tunnel.Service = "http://traefik:8081"
	if _, err := New(context.Background(), c, "cfsync"); err != nil {
		t.Fatal(err)
	}
}
