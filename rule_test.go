package cfsync

import (
	"reflect"
	"testing"
)

func TestExtractHosts(t *testing.T) {
	tests := []struct {
		name    string
		rule    string
		want    []string
		skipped bool
	}{
		{"single", "Host(`app.example.com`)", []string{"app.example.com"}, false},
		{"double quotes", `Host("app.example.com")`, []string{"app.example.com"}, false},
		{"v2 multi-arg", "Host(`a.example.com`, `b.example.net`)", []string{"a.example.com", "b.example.net"}, false},
		{"v3 or", "Host(`a.example.com`) || Host(`b.example.com`)", []string{"a.example.com", "b.example.com"}, false},
		{"with path", "Host(`app.example.com`) && PathPrefix(`/api`)", []string{"app.example.com"}, false},
		{"nested parens", "(Host(`a.example.com`) || Host(`b.example.com`)) && Method(`GET`)", []string{"a.example.com", "b.example.com"}, false},
		{"uppercase and port", "Host(`App.Example.COM:8443`)", []string{"app.example.com"}, false},
		{"trailing dot", "Host(`app.example.com.`)", []string{"app.example.com"}, false},
		{"dedupe", "Host(`a.example.com`) || Host(`A.example.com`)", []string{"a.example.com"}, false},
		{"wildcard skipped", "Host(`*.example.com`)", nil, true},
		{"regexp skipped", "HostRegexp(`^.+\\.example\\.com$`)", nil, true},
		{"regexp with host", "HostRegexp(`{sub:[a-z]+}.example.com`) || Host(`x.example.com`)", []string{"x.example.com"}, true},
		{"path only", "PathPrefix(`/`)", nil, false},
		{"no dot", "Host(`localhost`)", nil, true},
		{"not a Host matcher", "MyHost(`a.example.com`)", nil, false},
		{"header value is not host", "Header(`Host`, `a.example.com`)", nil, false},
		{"malformed", "Host(`a.example.com`", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, skipped := extractHosts(tt.rule)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("hosts = %v, want %v", got, tt.want)
			}
			if skipped != tt.skipped {
				t.Errorf("skipped = %v, want %v", skipped, tt.skipped)
			}
		})
	}
}
