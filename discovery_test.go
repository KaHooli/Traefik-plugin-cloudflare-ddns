package cfsync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
)

func TestListHTTPRoutersPaginates(t *testing.T) {
	pages := [][]router{
		{{Name: "a@docker", Rule: "Host(`a.example.com`)"}},
		{{Name: "b@file", Rule: "Host(`b.example.net`)"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/http/routers" {
			http.NotFound(w, r)
			return
		}
		if u, p, ok := r.BasicAuth(); !ok || u != "user" || p != "pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 || page > len(pages) {
			page = 1
		}
		if page < len(pages) {
			w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
		} else {
			w.Header().Set("X-Next-Page", "1")
		}
		_ = json.NewEncoder(w).Encode(pages[page-1])
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.TraefikAPI.URL = srv.URL + "/api/"
	cfg.TraefikAPI.Username = "user"
	cfg.TraefikAPI.Password = "pass"
	s, err := cfg.validate()
	if err != nil {
		t.Fatal(err)
	}

	got, err := newAPIClient(s).listHTTPRouters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "a@docker" || got[1].Name != "b@file" {
		t.Fatalf("unexpected routers: %+v", got)
	}
}

func TestListHTTPRoutersError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.TraefikAPI.URL = srv.URL
	s, err := cfg.validate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newAPIClient(s).listHTTPRouters(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestCollectHosts(t *testing.T) {
	routers := []router{
		{Name: "app@docker", Provider: "docker", Status: "enabled", EntryPoints: []string{"websecure"}, Rule: "Host(`app.example.com`)"},
		{Name: "app-alt@file", Provider: "file", Status: "enabled", EntryPoints: []string{"websecure"}, Rule: "Host(`app.example.com`) || Host(`app.example.net`)"},
		{Name: "tunnel@file", Provider: "file", Status: "enabled", EntryPoints: []string{"tunnel"}, Rule: "Host(`t.example.com`)"},
		{Name: "broken@docker", Provider: "docker", Status: "disabled", EntryPoints: []string{"websecure"}, Rule: "Host(`broken.example.com`)"},
		{Name: "dashboard@internal", Provider: "internal", Status: "enabled", Rule: "PathPrefix(`/api`)"},
		{Name: "wild@file", Provider: "file", Status: "enabled", Rule: "HostRegexp(`.+`)"},
	}

	tests := []struct {
		name        string
		entryPoints []string
		providers   []string
		wantHosts   []string
		wantSkipped []string
	}{
		{"all", nil, nil, []string{"app.example.com", "app.example.net", "t.example.com"}, []string{"wild@file"}},
		{"entrypoint filter", []string{"tunnel"}, nil, []string{"t.example.com"}, nil},
		{"provider filter", nil, []string{"Docker"}, []string{"app.example.com"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := CreateConfig()
			cfg.EntryPoints = tt.entryPoints
			cfg.Providers = tt.providers
			s, err := cfg.validate()
			if err != nil {
				t.Fatal(err)
			}
			hosts, skipped := collectHosts(routers, s)
			var names []string
			for _, h := range hosts {
				names = append(names, h.Host)
			}
			if !reflect.DeepEqual(names, tt.wantHosts) {
				t.Errorf("hosts = %v, want %v", names, tt.wantHosts)
			}
			if !reflect.DeepEqual(skipped, tt.wantSkipped) {
				t.Errorf("skipped = %v, want %v", skipped, tt.wantSkipped)
			}
		})
	}

	s, _ := CreateConfig().validate()
	hosts, _ := collectHosts(routers, s)
	if !reflect.DeepEqual(hosts[0].Routers, []string{"app-alt@file", "app@docker"}) {
		t.Errorf("routers for app.example.com = %v", hosts[0].Routers)
	}
}

// Regression test for a Yaegi bug: named result variables keep their value
// between calls, so repeated polls returned ever-growing host lists.
func TestCollectHostsRepeatable(t *testing.T) {
	s, err := CreateConfig().validate()
	if err != nil {
		t.Fatal(err)
	}
	routers := []router{
		{Name: "a@file", Status: "enabled", Rule: "Host(`a.example.com`)"},
		{Name: "w@file", Status: "enabled", Rule: "HostRegexp(`.+`)"},
	}
	first, firstSkipped := collectHosts(routers, s)
	second, secondSkipped := collectHosts(routers, s)
	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(firstSkipped, secondSkipped) {
		t.Fatalf("results differ between calls:\n%v %v\n%v %v", first, firstSkipped, second, secondSkipped)
	}
	if hosts, _ := extractHosts("Path(`/`)"); hosts != nil {
		t.Fatalf("extractHosts after other calls = %v, want nil", hosts)
	}
}
