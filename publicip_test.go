package cfsync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func ipServer(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(func() { srv.Close() })
	return srv.URL
}

func TestDetectPublicIPv4(t *testing.T) {
	down := ipServer(t, http.StatusBadGateway, "")
	v6 := ipServer(t, http.StatusOK, "2001:db8::1")
	private := ipServer(t, http.StatusOK, "192.168.1.10")
	cgnat := ipServer(t, http.StatusOK, "100.72.1.1")
	garbage := ipServer(t, http.StatusOK, "<html>")
	good := ipServer(t, http.StatusOK, "203.0.113.10\n")

	ip, err := detectPublicIPv4(context.Background(), http.DefaultClient, []string{down, v6, private, cgnat, garbage, good})
	if err != nil || ip != "203.0.113.10" {
		t.Fatalf("ip=%q err=%v", ip, err)
	}

	_, err = detectPublicIPv4(context.Background(), http.DefaultClient, []string{down, private})
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") || !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("err = %v", err)
	}
}
