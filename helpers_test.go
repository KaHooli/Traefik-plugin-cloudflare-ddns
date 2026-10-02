package cfsync

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	testAccount = "acc1"
	testTunnel  = "6ff42ae2-765d-4adf-8112-31c55c1551ef"
	testTarget  = testTunnel + ".cfargotunnel.com"
)

// testConfig is CreateConfig with a token and tunnel set, so validation passes.
func testConfig() *Config {
	c := CreateConfig()
	c.Cloudflare.APIToken = "test-token"
	c.Cloudflare.AccountID = testAccount
	c.Tunnel.ID = testTunnel
	c.Tunnel.Service = "http://traefik:8081"
	return c
}

func mustSettings(t *testing.T, c *Config) *settings {
	t.Helper()
	s, err := c.validate()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeCloudflare is an in-memory Cloudflare API with the endpoints the
// plugin uses. Small page sizes are honoured so pagination is exercised.
type fakeCloudflare struct {
	t      *testing.T
	mu     sync.Mutex
	zones  []zone
	recs   map[string][]dnsRecord // by zone ID
	nextID int
	writes []string
	srv    *httptest.Server

	tunnel       map[string]any
	tunnelConfig map[string]json.RawMessage
	tunnelPuts   int
	failPut      bool
}

func newFakeCloudflare(t *testing.T, zones ...zone) *fakeCloudflare {
	f := &fakeCloudflare{t: t, zones: zones, recs: make(map[string][]dnsRecord),
		tunnel: map[string]any{"id": testTunnel, "name": "home", "status": "healthy", "remote_config": true}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(func() { f.srv.Close() })
	return f
}

func (f *fakeCloudflare) add(zoneID string, r dnsRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	r.ID = "rec" + strconv.Itoa(f.nextID)
	f.recs[zoneID] = append(f.recs[zoneID], r)
}

func (f *fakeCloudflare) records(zoneID string) []dnsRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]dnsRecord(nil), f.recs[zoneID]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (f *fakeCloudflare) writeLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

func (f *fakeCloudflare) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.Header.Get("Authorization") != "Bearer test-token" {
		writeCF(w, http.StatusForbidden, false, nil, nil, "9109 Invalid access token")
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) >= 4 && parts[0] == "accounts" && parts[2] == "cfd_tunnel":
		f.handleTunnel(w, r, parts)

	case r.Method == http.MethodGet && len(parts) == 1 && parts[0] == "zones":
		f.page(w, r, f.zones, len(f.zones))

	case len(parts) >= 3 && parts[0] == "zones" && parts[2] == "dns_records":
		zoneID := parts[1]
		switch {
		case r.Method == http.MethodGet && len(parts) == 3:
			f.page(w, r, f.recs[zoneID], len(f.recs[zoneID]))
		case r.Method == http.MethodPost && len(parts) == 3:
			var rec dnsRecord
			_ = json.NewDecoder(r.Body).Decode(&rec)
			if rec.ID != "" {
				f.t.Errorf("create sent an id: %+v", rec)
			}
			f.nextID++
			rec.ID = "rec" + strconv.Itoa(f.nextID)
			f.recs[zoneID] = append(f.recs[zoneID], rec)
			f.writes = append(f.writes, fmt.Sprintf("create %s %s", rec.Name, rec.Content))
			writeCF(w, http.StatusOK, true, rec, nil, "")
		case r.Method == http.MethodPut && len(parts) == 4:
			var rec dnsRecord
			_ = json.NewDecoder(r.Body).Decode(&rec)
			for i, old := range f.recs[zoneID] {
				if old.ID == parts[3] {
					rec.ID = old.ID
					f.recs[zoneID][i] = rec
					f.writes = append(f.writes, fmt.Sprintf("update %s %s", rec.Name, rec.Content))
					writeCF(w, http.StatusOK, true, rec, nil, "")
					return
				}
			}
			writeCF(w, http.StatusNotFound, false, nil, nil, "81044 Record does not exist")
		case r.Method == http.MethodDelete && len(parts) == 4:
			for i, old := range f.recs[zoneID] {
				if old.ID == parts[3] {
					f.recs[zoneID] = append(f.recs[zoneID][:i], f.recs[zoneID][i+1:]...)
					f.writes = append(f.writes, fmt.Sprintf("delete %s %s", old.Name, old.Content))
					writeCF(w, http.StatusOK, true, map[string]string{"id": old.ID}, nil, "")
					return
				}
			}
			writeCF(w, http.StatusNotFound, false, nil, nil, "81044 Record does not exist")
		default:
			http.NotFound(w, r)
		}

	default:
		http.NotFound(w, r)
	}
}

// page serves one page of items. The fake caps per_page at 2 so tests hit
// several pages.
func (f *fakeCloudflare) page(w http.ResponseWriter, r *http.Request, items any, n int) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	const perPage = 2
	totalPages := (n + perPage - 1) / perPage
	start, end := (page-1)*perPage, page*perPage
	if start > n {
		start = n
	}
	if end > n {
		end = n
	}

	var slice any
	switch v := items.(type) {
	case []zone:
		slice = append([]zone{}, v[start:end]...)
	case []dnsRecord:
		slice = append([]dnsRecord{}, v[start:end]...)
	}
	writeCF(w, http.StatusOK, true, slice, &cfResultInfo{Page: page, TotalPages: totalPages}, "")
}

func writeCF(w http.ResponseWriter, status int, success bool, result any, info *cfResultInfo, errMsg string) {
	body := map[string]any{"success": success, "errors": []any{}, "result": result}
	if info != nil {
		body["result_info"] = info
	}
	if errMsg != "" {
		code, msg, _ := strings.Cut(errMsg, " ")
		n, _ := strconv.Atoi(code)
		body["errors"] = []cfError{{Code: n, Message: msg}}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// setIngress sets the tunnel's ingress rules (JSON objects).
func (f *fakeCloudflare) setIngress(rules ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tunnelConfig = map[string]json.RawMessage{
		"ingress":       json.RawMessage("[" + strings.Join(rules, ",") + "]"),
		"warp-routing":  json.RawMessage(`{"enabled":false}`),
		"originRequest": json.RawMessage(`{"connectTimeout":30}`),
	}
}

// ingress returns the current rules as "hostname=service" strings.
func (f *fakeCloudflare) ingress() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rules []map[string]any
	_ = json.Unmarshal(f.tunnelConfig["ingress"], &rules)
	var out []string
	for _, r := range rules {
		s := fmt.Sprint(r["hostname"], "=", r["service"])
		if p, ok := r["path"]; ok {
			s += " path=" + fmt.Sprint(p)
		}
		if o, ok := r["originRequest"].(map[string]any); ok && len(o) > 0 {
			b, _ := json.Marshal(o)
			s += " " + string(b)
		}
		out = append(out, s)
	}
	return out
}

func (f *fakeCloudflare) handleTunnel(w http.ResponseWriter, r *http.Request, parts []string) {
	if parts[1] != testAccount || parts[3] != testTunnel {
		writeCF(w, http.StatusNotFound, false, nil, nil, "1003 tunnel not found")
		return
	}
	switch {
	case len(parts) == 4 && r.Method == http.MethodGet:
		writeCF(w, http.StatusOK, true, f.tunnel, nil, "")
	case len(parts) == 5 && parts[4] == "configurations" && r.Method == http.MethodGet:
		cfg := map[string]json.RawMessage{}
		for k, v := range f.tunnelConfig {
			cfg[k] = v
		}
		// Like Cloudflare, return every rule with an originRequest object.
		if ing, ok := cfg["ingress"]; ok {
			var rules []map[string]any
			_ = json.Unmarshal(ing, &rules)
			for _, rule := range rules {
				if _, ok := rule["originRequest"]; !ok {
					rule["originRequest"] = map[string]any{}
				}
			}
			cfg["ingress"], _ = json.Marshal(rules)
		}
		var result map[string]any
		if f.tunnelConfig == nil {
			result = map[string]any{"tunnel_id": testTunnel, "config": nil}
		} else {
			result = map[string]any{"tunnel_id": testTunnel, "config": cfg}
		}
		writeCF(w, http.StatusOK, true, result, nil, "")
	case len(parts) == 5 && parts[4] == "configurations" && r.Method == http.MethodPut:
		if f.failPut {
			writeCF(w, http.StatusInternalServerError, false, nil, nil, "1000 internal error")
			return
		}
		var body struct {
			Config map[string]json.RawMessage `json:"config"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var rules []map[string]any
		_ = json.Unmarshal(body.Config["ingress"], &rules)
		if len(rules) == 0 || rules[len(rules)-1]["hostname"] != nil {
			writeCF(w, http.StatusBadRequest, false, nil, nil, "1055 last ingress rule must be a catch-all")
			return
		}
		f.tunnelConfig = body.Config
		f.tunnelPuts++
		f.writes = append(f.writes, "tunnel put")
		writeCF(w, http.StatusOK, true, map[string]any{"tunnel_id": testTunnel}, nil, "")
	default:
		http.NotFound(w, r)
	}
}
