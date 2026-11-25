package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestMetricsRoute(t *testing.T) {
	s := New("test", nil, nil, nil, nil, nil, nil, nil, nil, "", zap.NewNop())
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "strangle_store_up 0") {
		t.Errorf("metrics body = %s, want the store-down gauge", body)
	}
	if !strings.Contains(body, "# TYPE go_goroutines gauge") {
		t.Errorf("metrics body = %s, want Prometheus runtime metrics", body)
	}
}
