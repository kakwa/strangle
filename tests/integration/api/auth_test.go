package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kakwa/strangle/api"
	"github.com/kakwa/strangle/auth"
	"github.com/kakwa/strangle/playbook_applier"
	"github.com/kakwa/strangle/store"
	"github.com/kakwa/strangle/tests/testutil"
	"go.uber.org/zap"
)

// newAuthTestServer builds a server with DB-backed users but no token store.
// An empty users table keeps the API open.
func newAuthTestServer(t *testing.T, d *store.DB) *api.Server {
	t.Helper()
	apr := playbook_applier.NewAnsible(testTargets, stubExecutor{}, d, zap.NewNop())
	return api.New("test", apr, d, nil, nil, nil, d.Users(), nil, "", zap.NewNop())
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

// setCookieFrom pulls the session cookie out of a response, or nil.
func setCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == api.SessionCookie {
			return c
		}
	}
	return nil
}

// loginCookie logs in through the versioned endpoint and returns the cookie.
func loginCookie(t *testing.T, s *api.Server, user, pw string) *http.Cookie {
	t.Helper()
	rec := postJSON(t, s, "/api/v1/login",
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
	s := newTestServer(t) // no user store

	rec := doGet(t, s, "/api/v1/me")
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
	if rec := doGet(t, s, "/api/v1/orgs"); rec.Code != http.StatusOK {
		t.Errorf("/api/v1/orgs = %d, want %d", rec.Code, http.StatusOK)
	}
	assertAPIGone(t, doGet(t, s, "/api/me"), "GET /api/me")
}

func TestMeWithEmptyUserStore(t *testing.T) {
	d := testutil.OpenDB(t, "server")
	defer d.Close()
	s := newAuthTestServer(t, d) // store, but no users

	rec := doGet(t, s, "/api/v1/me")
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
	rec = postJSON(t, s, "/api/v1/login", `{"username":"a","password":"b"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("login = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if rec := doGet(t, s, "/api/v1/orgs"); rec.Code != http.StatusOK {
		t.Errorf("/api/v1/orgs = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestLoginFlow(t *testing.T) {
	d := testutil.OpenDB(t, "server")
	defer d.Close()
	seedUser(t, d.Users(), "alice", "s3cret")
	s := newAuthTestServer(t, d)

	// Gated without a session.
	if rec := doGet(t, s, "/api/v1/orgs"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/v1/orgs without session = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// Wrong password and unknown user get the same 401 (no enumeration).
	for _, body := range []string{
		`{"username":"alice","password":"nope"}`,
		`{"username":"mallory","password":"s3cret"}`,
	} {
		if rec := postJSON(t, s, "/api/v1/login", body, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("login %s = %d, want %d", body, rec.Code, http.StatusUnauthorized)
		}
	}
	if rec := postJSON(t, s, "/api/v1/login", `{not json`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("login bad body = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	// Successful login sets an HttpOnly Lax cookie and returns the identity.
	rec := postJSON(t, s, "/api/v1/login", `{"username":"alice","password":"s3cret"}`, nil)
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

	h := map[string]string{"Cookie": c.String()}
	rec = getWith(t, s, "/api/v1/me", h)
	if rec.Code != http.StatusOK || json.NewDecoder(rec.Body).Decode(&me) != nil {
		t.Fatalf("GET /api/v1/me = %d, body %s", rec.Code, rec.Body.String())
	}
	if !me.Authenticated || me.Username != "alice" {
		t.Errorf("me = %+v, want authenticated alice", me)
	}
	if rec := getWith(t, s, "/api/v1/orgs", h); rec.Code != http.StatusOK {
		t.Errorf("/api/v1/orgs with session = %d, want %d", rec.Code, http.StatusOK)
	}

	// Health stays open even when auth is on.
	if rec := doGet(t, s, "/api/v1/health"); rec.Code != http.StatusOK {
		t.Errorf("/api/v1/health = %d, want %d", rec.Code, http.StatusOK)
	}

	// Logout kills the session.
	if rec := postJSON(t, s, "/api/v1/logout", "", h); rec.Code != http.StatusOK {
		t.Fatalf("logout = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec := getWith(t, s, "/api/v1/orgs", h); rec.Code != http.StatusUnauthorized {
		t.Errorf("/api/v1/orgs with logged-out cookie = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func newHookAuthTestServer(t *testing.T, d *store.DB, syncer api.Syncer, token string) *api.Server {
	t.Helper()
	apr := playbook_applier.NewAnsible(testTargets, stubExecutor{}, d, zap.NewNop())
	return api.New("test", apr, d, nil, nil, syncer, d.Users(), nil, token, zap.NewNop())
}

func TestHookWithSession(t *testing.T) {
	st := testutil.OpenDB(t, "server")
	defer st.Close()
	seedUser(t, st.Users(), "alice", "s3cret")

	fake := &fakeSyncer{rev: "abc123", changed: true, targets: testTargets}
	s := newHookAuthTestServer(t, st, fake, "")
	path := "/api/v1/orgs/default/collections/home/hook"

	// Auth on, no credentials: the hook is rejected.
	if rec := postJSON(t, s, path, `{}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("hook without session = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	c := loginCookie(t, s, "alice", "s3cret")
	rec := postJSON(t, s, path, `{"branch":"develop"}`, map[string]string{"Cookie": c.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("hook with session = %d, want %d, body %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if fake.called != 1 {
		t.Errorf("syncer called %d times, want 1", fake.called)
	}
}

func TestStaticTokenIsHookOnly(t *testing.T) {
	st := testutil.OpenDB(t, "server")
	defer st.Close()
	seedUser(t, st.Users(), "alice", "s3cret")

	fake := &fakeSyncer{rev: "abc123", targets: testTargets}
	s := newHookAuthTestServer(t, st, fake, "sekrit")
	path := "/api/v1/orgs/default/collections/home/hook"

	// The static machine token works on the hook with both header styles.
	for _, h := range []map[string]string{
		{"Authorization": "Bearer sekrit"},
		{"X-Strangle-Token": "sekrit"},
	} {
		if rec := postJSON(t, s, path, `{}`, h); rec.Code != http.StatusOK {
			t.Errorf("hook with %v: status = %d, want %d (body %s)", h, rec.Code, http.StatusOK, rec.Body.String())
		}
	}
	if rec := postJSON(t, s, path, `{}`, map[string]string{"X-Strangle-Token": "wrong"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("hook with wrong token = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if fake.called != 2 {
		t.Errorf("syncer called %d times, want 2", fake.called)
	}

	// It is not a user identity: regular API routes stay closed.
	for _, h := range []map[string]string{
		{"X-Strangle-Token": "sekrit"},
		{"Authorization": "Bearer sekrit"},
	} {
		if rec := getWith(t, s, "/api/v1/orgs", h); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET orgs with static token = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	}
}
