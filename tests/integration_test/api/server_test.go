package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kakwa/strangle/api"
	"github.com/kakwa/strangle/controller/applier"
	"github.com/kakwa/strangle/tests/testutil"
	"go.uber.org/zap"
)

// stubExecutor satisfies applier.Executor. The HTTP server does not execute
// playbooks; it only records their results in the store.
type stubExecutor struct{}

func (stubExecutor) RunPlaybook(context.Context, string, string, string, ...string) (int, string, map[string]json.RawMessage, error) {
	return 0, "", nil, nil
}

// newTestServer builds a server with the package's standard test targets over
// a fresh test database.
func newTestServer(t *testing.T) *api.Server {
	t.Helper()
	st := testutil.OpenDB(t, "server")
	apr := applier.NewAnsible(testTargets, stubExecutor{}, st, zap.NewNop())
	return api.New("test", apr, st, nil, nil, nil, nil, nil, "", zap.NewNop())
}

// request serves one request through the full route table.
func request(t *testing.T, s *api.Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func doGet(t *testing.T, s *api.Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, s, http.MethodGet, path, "", nil)
}

func getWith(t *testing.T, s *api.Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, s, http.MethodGet, path, "", headers)
}

func postJSON(t *testing.T, s *api.Server, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, s, http.MethodPost, path, body, headers)
}

func patchJSON(t *testing.T, s *api.Server, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, s, http.MethodPatch, path, body, headers)
}

func deleteWith(t *testing.T, s *api.Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, s, http.MethodDelete, path, "", headers)
}

// assertAPIGone checks that a path formerly served by a flat API alias is no
// longer an API route: unmatched paths fall through to the SPA handler (which
// serves index.html) or are rejected by the router itself.
func assertAPIGone(t *testing.T, rec *httptest.ResponseRecorder, label string) {
	t.Helper()
	ct := rec.Header().Get("Content-Type")
	if rec.Code == http.StatusNotFound || strings.Contains(ct, "text/html") {
		return
	}
	t.Errorf("%s: status = %d, content type = %q; want a 404 or the SPA fallback, not an API response (body %s)",
		label, rec.Code, ct, rec.Body.String())
}
