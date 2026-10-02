package cfsync

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestConfigValidation(t *testing.T) {
	bad := []func(c *Config){
		func(c *Config) { c.PollInterval = "1s" },
		func(c *Config) { c.PollInterval = "soon" },
		func(c *Config) { c.TraefikAPI.URL = "127.0.0.1:8080/api" },
		func(c *Config) { c.TraefikAPI.Timeout = "x" },
	}
	for i, mutate := range bad {
		c := CreateConfig()
		mutate(c)
		if _, err := New(context.Background(), c, "test"); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
	if _, err := New(context.Background(), CreateConfig(), "test"); err != nil {
		t.Errorf("default config: %v", err)
	}
}

// The API is not up when Provide is called; the provider must retry, then
// log the discovered hosts, and Stop must return promptly.
func TestProviderLifecycle(t *testing.T) {
	var ready atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode([]router{{Name: "app@file", Provider: "file", Status: "enabled", Rule: "Host(`app.example.com`)"}})
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.TraefikAPI.URL = srv.URL
	p, err := New(context.Background(), cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	p.logger = log.New(out, "", 0)

	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.Provide(make(chan json.Marshaler)); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)
	ready.Store(true)

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "app.example.com <- app@file") {
		if time.Now().After(deadline) {
			t.Fatalf("host not logged; output:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "waiting for Traefik API") {
		t.Errorf("expected a retry log line; output:\n%s", out.String())
	}

	done := make(chan struct{})
	go func() { _ = p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}
}
