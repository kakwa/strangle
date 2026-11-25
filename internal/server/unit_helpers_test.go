package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

type helperEnqueuer struct{}

func (helperEnqueuer) EnqueueRun(context.Context, string, string, uint64, bool) (string, error) {
	return "1", nil
}

func TestIntQuery(t *testing.T) {
	cases := []struct {
		q    string
		def  int
		want int
	}{
		{"", 50, 50},
		{"0", 50, 50},
		{"-1", 50, 50},
		{"abc", 50, 50},
		{"7", 50, 7},
	}
	for _, tc := range cases {
		u := "/api/runs"
		if tc.q != "" {
			u = "/api/runs?limit=" + tc.q
		}
		req := httptest.NewRequest(http.MethodGet, u, nil)
		if got := intQuery(req, "limit", tc.def); got != tc.want {
			t.Errorf("intQuery(%q, %d) = %d, want %d", tc.q, tc.def, got, tc.want)
		}
	}
}

func TestBearerTokenOK(t *testing.T) {
	s := New("test", nil, nil, nil, helperEnqueuer{}, nil, nil, nil, nil, "sekrit", zap.NewNop())

	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"no headers", nil, false},
		{"empty bearer", map[string]string{"Authorization": "Bearer "}, false},
		{"wrong token", map[string]string{"Authorization": "Bearer wrong"}, false},
		{"right bearer", map[string]string{"Authorization": "Bearer sekrit"}, true},
		{"right webhook header", map[string]string{"X-Strangle-Token": "sekrit"}, true},
		{"webhook header ignored when bearer present", map[string]string{"Authorization": "Bearer wrong", "X-Strangle-Token": "sekrit"}, false},
		{"non-bearer auth scheme", map[string]string{"Authorization": "Basic aWQ9"}, false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/sync", strings.NewReader("{}"))
		for k, v := range tc.headers {
			req.Header.Set(k, v)
		}
		if got := s.bearerTokenOK(req); got != tc.want {
			t.Errorf("%s: bearerTokenOK() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
