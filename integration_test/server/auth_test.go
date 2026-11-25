package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kakwa/strangle/internal/server"
	"github.com/kakwa/strangle/internal/testutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/auth"
	"go.uber.org/zap"
)

// newAuthTestServer builds a Server with one target and the given user
// store (nil keeps the server open).
func newAuthTestServer(t *testing.T, users auth.UserStore) *server.Server {
	t.Helper()
	st := testutil.OpenDB(t, "server")
	apr := applier.NewAnsible(testTargets, stubExecutor{}, st, zap.NewNop())
	return server.New("test", apr, st, nil, &recEnqueuer{status: "queued"}, nil, nil, users, nil, "", zap.NewNop())
}

func seedUser(t *testing.T, users auth.UserStore, name, pw string) {
	t.Helper()
	hash, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}
	if err := users.CreateUser(context.Background(), name, hash); err != nil {
		t.Fatalf("CreateUser() error: %v", err)
	}
}

func postAuthJSON(t *testing.T, s *server.Server, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func getAuthWith(t *testing.T, s *server.Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

// setCookieFrom pulls the session cookie out of a response, or nil.
func setCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == server.SessionCookie {
			return c
		}
	}
	return nil
}

// loginCookie logs in and returns the session cookie.
func loginCookie(t *testing.T, s *server.Server, user, pw string) *http.Cookie {
	t.Helper()
	rec := postAuthJSON(t, s, "/api/login",
		fmt.Sprintf(`{"username":%q,"password":%q}`, user, pw), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %q: status = %d, body %s", user, rec.Code, rec.Body.String())
	}
	c := setCookieFrom(t, rec)
	if c == nil {
		t.Fatal("login did not set a session cookie")
	}
	return c
}

func TestMeWhenAuthOff(t *testing.T) {
	s := newTestServer(t, &recEnqueuer{status: "queued"}) // no user store

	rec := doGet(t, s, "/api/me")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var me meResponse
	if err := json.NewDecoder(rec.Body).Decode(&me); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if me.Authenticated || me.Username != "" {
		t.Errorf("me = %+v, want unauthenticated", me)
	}

	// Data endpoints stay open.
	if rec := doGet(t, s, "/api/stats"); rec.Code != http.StatusOK {
		t.Errorf("/api/stats = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestMeWithEmptyUserStore(t *testing.T) {
	s := newAuthTestServer(t, auth.NewMemUserStore()) // store, but no users

	rec := doGet(t, s, "/api/me")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var me meResponse
	if err := json.NewDecoder(rec.Body).Decode(&me); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if me.Authenticated {
		t.Errorf("me = %+v, want unauthenticated", me)
	}

	// No users: login must fail clearly, not hang the UI.
	rec = postAuthJSON(t, s, "/api/login", `{"username":"a","password":"b"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("login = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// The API stays open until the first user is created.
	if rec := doGet(t, s, "/api/stats"); rec.Code != http.StatusOK {
		t.Errorf("/api/stats = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestLoginFlow(t *testing.T) {
	users := auth.NewMemUserStore()
	seedUser(t, users, "alice", "s3cret")
	s := newAuthTestServer(t, users)

	// Gated without a session.
	if rec := doGet(t, s, "/api/stats"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/stats without session = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// Wrong password and unknown user both get the same 401 (no account
	// enumeration).
	if rec := postAuthJSON(t, s, "/api/login", `{"username":"alice","password":"nope"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("login wrong password = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if rec := postAuthJSON(t, s, "/api/login", `{"username":"mallory","password":"s3cret"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("login unknown user = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if rec := postAuthJSON(t, s, "/api/login", `{not json`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("login bad body = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	// Successful login sets an HttpOnly Lax cookie.
	rec := postAuthJSON(t, s, "/api/login", `{"username":"alice","password":"s3cret"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d, want %d, body %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	c := setCookieFrom(t, rec)
	if c == nil {
		t.Fatal("login did not set a session cookie")
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie = %+v, want HttpOnly SameSite=Lax", c)
	}
	var me meResponse
	if err := json.NewDecoder(rec.Body).Decode(&me); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !me.Authenticated || me.Username != "alice" {
		t.Errorf("login body = %+v, want authenticated alice", me)
	}

	// The cookie unlocks the API.
	rec = getAuthWith(t, s, "/api/me", map[string]string{"Cookie": c.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/me with session = %d, want %d", rec.Code, http.StatusOK)
	}
	if err := json.NewDecoder(rec.Body).Decode(&me); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !me.Authenticated || me.Username != "alice" {
		t.Errorf("me = %+v, want authenticated alice", me)
	}
	if rec := getAuthWith(t, s, "/api/stats", map[string]string{"Cookie": c.String()}); rec.Code != http.StatusOK {
		t.Errorf("/api/stats with session = %d, want %d", rec.Code, http.StatusOK)
	}

	// Health stays open even when auth is on.
	if rec := doGet(t, s, "/api/health"); rec.Code != http.StatusOK {
		t.Errorf("/api/health = %d, want %d", rec.Code, http.StatusOK)
	}

	// Logout kills the token.
	if rec := postAuthJSON(t, s, "/api/logout", "", map[string]string{"Cookie": c.String()}); rec.Code != http.StatusOK {
		t.Fatalf("logout = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec := getAuthWith(t, s, "/api/stats", map[string]string{"Cookie": c.String()}); rec.Code != http.StatusUnauthorized {
		t.Errorf("/api/stats with logged-out cookie = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestSyncWithSession(t *testing.T) {
	users := auth.NewMemUserStore()
	seedUser(t, users, "alice", "s3cret")

	fake := &fakeSyncer{
		rev:     "abc123",
		changed: true,
		targets: testTargets,
	}
	st := testutil.OpenDB(t, "server")
	apr := applier.NewAnsible(testTargets, stubExecutor{}, st, zap.NewNop())
	s := server.New("test", apr, st, nil, &recEnqueuer{status: "queued"}, nil, fake, users, nil, "", zap.NewNop())

	// Auth on, no token configured: a bare sync is rejected.
	if rec := postSync(t, s, `{"target":"home"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("sync without session = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// A valid session is accepted.
	c := loginCookie(t, s, "alice", "s3cret")
	rec := postSync(t, s, `{"target":"home"}`, map[string]string{"Cookie": c.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("sync with session = %d, want %d, body %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if fake.called != 1 {
		t.Errorf("syncer called %d times, want 1", fake.called)
	}
}

func TestSyncTokenWithAuthOn(t *testing.T) {
	users := auth.NewMemUserStore()
	seedUser(t, users, "alice", "s3cret")

	fake := &fakeSyncer{rev: "abc123", targets: testTargets}
	st := testutil.OpenDB(t, "server")
	apr := applier.NewAnsible(testTargets, stubExecutor{}, st, zap.NewNop())
	s := server.New("test", apr, st, nil, &recEnqueuer{status: "queued"}, nil, fake, users, nil, "sekrit", zap.NewNop())

	// The configured token still works for machine-to-machine syncs.
	rec := postSync(t, s, `{"target":"home"}`, map[string]string{"X-Strangle-Token": "sekrit"})
	if rec.Code != http.StatusOK {
		t.Fatalf("sync with token = %d, want %d, body %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// A wrong token does not.
	rec = postSync(t, s, `{"target":"home"}`, map[string]string{"X-Strangle-Token": "wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("sync with wrong token = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
