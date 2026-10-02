package cfsync

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestCloudflareClient(t *testing.T) {
	f := newFakeCloudflare(t, zoneCom, zoneNet, zoneSub)
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		f.add("z1", owned(n+".example.com", "203.0.113.10"))
	}
	c := newCloudflareClient(f.srv.URL, "test-token", http.DefaultClient)
	ctx := context.Background()

	zones, err := c.listZones(ctx)
	if err != nil || len(zones) != 3 {
		t.Fatalf("zones=%v err=%v", zones, err)
	}
	recs, err := c.listRecords(ctx, "z1")
	if err != nil || len(recs) != 5 {
		t.Fatalf("records=%d err=%v", len(recs), err)
	}

	if err := c.createRecord(ctx, "z2", dnsRecord{ID: "ignored", Type: "A", Name: "x.example.net", Content: "203.0.113.10"}); err != nil {
		t.Fatal(err)
	}
	r := recs[0]
	r.Content = "198.51.100.1"
	if err := c.updateRecord(ctx, "z1", r); err != nil {
		t.Fatal(err)
	}
	if err := c.deleteRecord(ctx, "z1", recs[1].ID); err != nil {
		t.Fatal(err)
	}
	want := "create x.example.net 203.0.113.10|update a.example.com 198.51.100.1|delete b.example.com 203.0.113.10"
	if got := strings.Join(f.writeLog(), "|"); got != want {
		t.Errorf("writes = %s", got)
	}

	err = c.deleteRecord(ctx, "z1", "missing")
	if err == nil || !strings.Contains(err.Error(), "81044") {
		t.Errorf("err = %v", err)
	}

	bad := newCloudflareClient(f.srv.URL, "wrong", http.DefaultClient)
	if _, err := bad.listZones(ctx); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("bad token err = %v", err)
	}
}
