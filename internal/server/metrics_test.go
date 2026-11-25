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
	if !strings.Contains(body, `strangle_workers{state="total"} 0`) {
		t.Errorf("metrics body = %s, want the zero-valued worker gauge", body)
	}
	if !strings.Contains(body, "strangle_build_info{version=\"dev\"} 1") {
		t.Errorf("metrics body = %s, want build info", body)
	}
	if !strings.Contains(body, "# TYPE go_goroutines gauge") {
		t.Errorf("metrics body = %s, want Prometheus runtime metrics", body)
	}
}
