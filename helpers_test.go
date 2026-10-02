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

// testConfig is CreateConfig with a token set, so validation passes.
func testConfig() *Config {
	c := CreateConfig()
	c.Cloudflare.APIToken = "test-token"
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
}

func newFakeCloudflare(t *testing.T, zones ...zone) *fakeCloudflare {
	f := &fakeCloudflare{t: t, zones: zones, recs: make(map[string][]dnsRecord)}
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
