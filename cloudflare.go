package cfsync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// zone is a Cloudflare zone.
type zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// dnsRecord is a Cloudflare DNS record.
type dnsRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
	Comment string `json:"comment"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfResultInfo struct {
	Page       int `json:"page"`
	TotalPages int `json:"total_pages"`
}

type cfEnvelope struct {
	Success    bool            `json:"success"`
	Errors     []cfError       `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *cfResultInfo   `json:"result_info"`
}

const cfMaxPages = 200

// cloudflareClient is a minimal client for the Cloudflare v4 API.
type cloudflareClient struct {
	base  string
	token string
	http  *http.Client
}

func newCloudflareClient(base, token string, client *http.Client) *cloudflareClient {
	return &cloudflareClient{base: base, token: token, http: client}
}

func (c *cloudflareClient) do(ctx context.Context, method, path string, body any) (*cfEnvelope, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}

	env := &cfEnvelope{}
	if err := json.Unmarshal(raw, env); err != nil {
		return nil, fmt.Errorf("%s %s: HTTP %d: invalid response", method, path, resp.StatusCode)
	}
	if !env.Success || resp.StatusCode >= 300 {
		var msgs []string
		for _, e := range env.Errors {
			msgs = append(msgs, fmt.Sprintf("%d %s", e.Code, e.Message))
		}
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.Join(msgs, "; "))
	}
	return env, nil
}

// listPaged calls GET path with page/per_page until all pages are read, and
// passes each page's result to add.
func (c *cloudflareClient) listPaged(ctx context.Context, path string, query url.Values, perPage int, add func(json.RawMessage) error) error {
	for page := 1; page <= cfMaxPages; page++ {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("page", strconv.Itoa(page))
		q.Set("per_page", strconv.Itoa(perPage))

		env, err := c.do(ctx, http.MethodGet, path+"?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		if err := add(env.Result); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		if env.ResultInfo == nil || page >= env.ResultInfo.TotalPages {
			return nil
		}
	}
	return fmt.Errorf("GET %s: more than %d pages", path, cfMaxPages)
}

func (c *cloudflareClient) listZones(ctx context.Context) ([]zone, error) {
	var zones []zone
	err := c.listPaged(ctx, "/zones", url.Values{}, 50, func(raw json.RawMessage) error {
		var page []zone
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		zones = append(zones, page...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return zones, nil
}

func (c *cloudflareClient) listRecords(ctx context.Context, zoneID string) ([]dnsRecord, error) {
	var records []dnsRecord
	err := c.listPaged(ctx, "/zones/"+url.PathEscape(zoneID)+"/dns_records", url.Values{}, 1000, func(raw json.RawMessage) error {
		var page []dnsRecord
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		records = append(records, page...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

func (c *cloudflareClient) createRecord(ctx context.Context, zoneID string, r dnsRecord) error {
	r.ID = ""
	_, err := c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/dns_records", r)
	return err
}

func (c *cloudflareClient) updateRecord(ctx context.Context, zoneID string, r dnsRecord) error {
	id := r.ID
	r.ID = ""
	_, err := c.do(ctx, http.MethodPut, "/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(id), r)
	return err
}

func (c *cloudflareClient) deleteRecord(ctx context.Context, zoneID, recordID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(recordID), nil)
	return err
}
