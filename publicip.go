package cfsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// detectPublicIPv4 asks each source in order and returns the first valid
// public IPv4 address.
func detectPublicIPv4(ctx context.Context, client *http.Client, sources []string) (string, error) {
	return detectPublicIP(ctx, client, sources, false)
}

// detectPublicIPv6 is detectPublicIPv4 for IPv6.
func detectPublicIPv6(ctx context.Context, client *http.Client, sources []string) (string, error) {
	return detectPublicIP(ctx, client, sources, true)
}

func detectPublicIP(ctx context.Context, client *http.Client, sources []string, v6 bool) (string, error) {
	family := "IPv4"
	if v6 {
		family = "IPv6"
	}
	var errs []string
	for _, src := range sources {
		ip, err := fetchIP(ctx, client, src, v6)
		if err == nil {
			return ip, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", src, err))
	}
	if len(errs) == 0 {
		return "", errors.New("no IP sources configured")
	}
	return "", fmt.Errorf("public %s detection failed: %s", family, strings.Join(errs, "; "))
}

func fetchIP(ctx context.Context, client *http.Client, src string, v6 bool) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	_ = resp.Body.Close()
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	text := strings.TrimSpace(string(body))
	ip := net.ParseIP(text)
	if v6 {
		if ip == nil || ip.To4() != nil {
			return "", fmt.Errorf("not an IPv6 address: %q", text)
		}
		if !isPublicIPv6(ip) {
			return "", fmt.Errorf("not a public address: %s", ip)
		}
		return ip.String(), nil
	}
	if ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("not an IPv4 address: %q", text)
	}
	if !isPublicIPv4(ip) {
		return "", fmt.Errorf("not a public address: %s", ip)
	}
	return ip.To4().String(), nil
}

// isPublicIPv6 rejects loopback, link-local, unique-local (fc00::/7) and
// other non-global addresses.
func isPublicIPv6(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate()
}

func isPublicIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	cgnat := ip4[0] == 100 && ip4[1]&0xc0 == 64 // 100.64.0.0/10
	return !cgnat && !ip4.IsPrivate() && !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast() &&
		!ip4.IsUnspecified() && !ip4.IsMulticast()
}
