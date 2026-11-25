package server_test

import (
	"context"
	"encoding/json"
	"github.com/kakwa/strangle/internal/server"
	"github.com/kakwa/strangle/internal/testutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/auth"
	"github.com/kakwa/strangle/internal/db"
	"go.uber.org/zap"
)

// newTokenTestServer builds a Server with DB-backed user and token stores.
func newTokenTestServer(t *testing.T, d *db.DB) *server.Server {
	t.Helper()
	apr := applier.NewAnsible(testTargets, stubExecutor{}, d, zap.NewNop())
	return server.New("test", apr, d, nil, &recEnqueuer{status: "queued"}, nil, nil, d.Users(), d.Tokens(), "", zap.NewNop())
}

func deleteAuth(t *testing.T, s *server.Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func TestTokensCRUDWithSession(t *testing.T) {
	d := testutil.OpenDB(t, "server")
	defer d.Close()
	seedUser(t, d.Users(), "alice", "s3cret")
	s := newTokenTestServer(t, d)

	c := loginCookie(t, s, "alice", "s3cret")
	h := map[string]string{"Cookie": c.String()}

	// Empty at first.
	rec := getAuthWith(t, s, "/api/tokens", h)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/tokens = %d, want %d", rec.Code, http.StatusOK)
	}
	var list []*auth.Token
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("initial list = %v, want empty", list)
	}

	// Create returns the raw value exactly once.
	rec = postAuthJSON(t, s, "/api/tokens", `{"name":"ci"}`, h)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/tokens = %d, want %d, body %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var created struct {
		Raw   string      `json:"raw"`
		Token *auth.Token `json:"token"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(created.Raw, auth.TokenPrefix) {
		t.Errorf("raw = %q, want %q prefix", created.Raw, auth.TokenPrefix)
	}
	if created.Token == nil || created.Token.Name != "ci" {
		t.Errorf("token record = %+v, want name ci", created.Token)
	}

	// A duplicate name fails with 409.
	rec = postAuthJSON(t, s, "/api/tokens", `{"name":"ci"}`, h)
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate POST = %d, want %d", rec.Code, http.StatusConflict)
	}

	// The listing shows the prefix, never the raw value.
	rec = getAuthWith(t, s, "/api/tokens", h)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after create = %d", rec.Code)
	}
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 || list[0].Name != "ci" {
		t.Fatalf("list = %+v, want one token named ci", list)
	}
	if strings.Contains(rec.Body.String(), created.Raw) {
		t.Errorf("listing leaked the raw token value")
	}

	// Revoke it.
	rec = deleteAuth(t, s, "/api/tokens/ci", h)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want %d", rec.Code, http.StatusNoContent)
	}
	rec = deleteAuth(t, s, "/api/tokens/ci", h)
	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE again = %d, want %d", rec.Code, http.StatusNotFound)
	}
	rec = getAuthWith(t, s, "/api/tokens", h)
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("list after remove = %+v, want empty", list)
	}

	// A bad body is a 400, not a panic.
	if rec := postAuthJSON(t, s, "/api/tokens", `{not json`, h); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestTokensRequireUser(t *testing.T) {
	d := testutil.OpenDB(t, "server")
	defer d.Close()
	seedUser(t, d.Users(), "alice", "s3cret")
	s := newTokenTestServer(t, d)

	// No identity at all: the API is gated before the handler runs.
	if rec := getAuthNoHeader(t, s, "/api/tokens"); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/tokens anonymous = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// A valid session can manage tokens.
	c := loginCookie(t, s, "alice", "s3cret")
	if rec := getAuthWith(t, s, "/api/tokens", map[string]string{"Cookie": c.String()}); rec.Code != http.StatusOK {
		t.Errorf("GET /api/tokens with session = %d, want %d", rec.Code, http.StatusOK)
	}
}

func getAuthNoHeader(t *testing.T, s *server.Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func TestTokenBearerAuthenticates(t *testing.T) {
	d := testutil.OpenDB(t, "server")
	defer d.Close()
	seedUser(t, d.Users(), "alice", "s3cret")
	s := newTokenTestServer(t, d)

	raw, _, err := d.Tokens().Create(context.Background(), "alice", "cli", 0)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	// The bearer token unlocks gated endpoints, via both header styles.
	if rec := getAuthWith(t, s, "/api/stats", map[string]string{"Authorization": "Bearer " + raw}); rec.Code != http.StatusOK {
		t.Errorf("/api/stats with bearer = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec := getAuthWith(t, s, "/api/stats", map[string]string{"X-Strangle-Token": raw}); rec.Code != http.StatusOK {
		t.Errorf("/api/stats with X-Strangle-Token = %d, want %d", rec.Code, http.StatusOK)
	}

	// A wrong or stale value does not.
	if rec := getAuthWith(t, s, "/api/stats", map[string]string{"Authorization": "Bearer wrong"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("/api/stats with wrong bearer = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// A revoked token stops working.
	if err := d.Tokens().Remove(context.Background(), "alice", "cli"); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}
	if rec := getAuthWith(t, s, "/api/stats", map[string]string{"Authorization": "Bearer " + raw}); rec.Code != http.StatusUnauthorized {
		t.Errorf("/api/stats with revoked bearer = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestStaticTokenIsNotAUser(t *testing.T) {
	st := testutil.OpenDB(t, "server")
	defer st.Close()
	seedUser(t, st.Users(), "alice", "s3cret")
	apr := applier.NewAnsible(testTargets, stubExecutor{}, st, zap.NewNop())
	s := server.New("test", apr, st, nil, &recEnqueuer{status: "queued"}, nil, nil, st.Users(), st.Tokens(), "sekrit", zap.NewNop())

	// The static sync token is not a user: it cannot pass the auth gate.
	rec := getAuthWith(t, s, "/api/tokens", map[string]string{"X-Strangle-Token": "sekrit"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/tokens with static token = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	rec = postAuthJSON(t, s, "/api/tokens", `{"name":"x"}`, map[string]string{"X-Strangle-Token": "sekrit"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/tokens with static token = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestTokensNoUserIdentity(t *testing.T) {
	// No users (auth off) but a token store exists: the endpoints are
	// reachable, yet without a user identity they are rejected.
	d := testutil.OpenDB(t, "server")
	defer d.Close()
	s := newTokenTestServer(t, d)

	if rec := getAuthNoHeader(t, s, "/api/tokens"); rec.Code != http.StatusForbidden {
		t.Errorf("GET /api/tokens anonymous = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if rec := postAuthJSON(t, s, "/api/tokens", `{"name":"x"}`, nil); rec.Code != http.StatusForbidden {
		t.Errorf("POST /api/tokens anonymous = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if rec := deleteAuth(t, s, "/api/tokens/x", nil); rec.Code != http.StatusForbidden {
		t.Errorf("DELETE /api/tokens anonymous = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestTokensWithoutStore(t *testing.T) {
	// No users, no tokens: auth is off, the endpoints report that token
	// storage is unavailable instead of panicking.
	s := newTestServer(t, &recEnqueuer{status: "queued"})

	if rec := getAuthNoHeader(t, s, "/api/tokens"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /api/tokens without store = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if rec := postAuthJSON(t, s, "/api/tokens", `{"name":"x"}`, nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("POST /api/tokens without store = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if rec := deleteAuth(t, s, "/api/tokens/x", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("DELETE /api/tokens without store = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
