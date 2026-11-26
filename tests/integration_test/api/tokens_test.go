package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kakwa/strangle/api"
	"github.com/kakwa/strangle/controller/applier"
	"github.com/kakwa/strangle/model"
	"github.com/kakwa/strangle/security/auth"
	"github.com/kakwa/strangle/tests/testutil"
	"go.uber.org/zap"
)

// newTokenTestServer builds a server with DB-backed user and token stores.
func newTokenTestServer(t *testing.T, d *model.DB) *api.Server {
	t.Helper()
	apr := applier.NewAnsible(orgTargets, stubExecutor{}, d, zap.NewNop())
	return api.New("test", apr, d, nil, nil, nil, d.Users(), d.Tokens(), "", zap.NewNop())
}

func TestTokensCRUDWithSession(t *testing.T) {
	d := testutil.OpenDB(t, "server")
	defer d.Close()
	seedUser(t, d.Users(), "alice", "s3cret")
	s := newTokenTestServer(t, d)

	c := loginCookie(t, s, "alice", "s3cret")
	h := map[string]string{"Cookie": c.String()}
	base := "/api/v1/orgs/default/collections/home/tokens"

	// Empty at first.
	rec := getWith(t, s, base, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d (body %s)", base, rec.Code, http.StatusOK, rec.Body.String())
	}
	var list []*auth.Token
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("initial list = %v, want empty", list)
	}

	// Create returns the raw value exactly once, scoped to the URL.
	rec = postJSON(t, s, base, `{"name":"ci"}`, h)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST %s = %d, want %d, body %s", base, rec.Code, http.StatusCreated, rec.Body.String())
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
	if created.Token == nil || created.Token.Name != "ci" ||
		created.Token.Org != "default" || created.Token.Collection != "home" {
		t.Errorf("token record = %+v, want ci scoped to default/home", created.Token)
	}

	// Duplicate names conflict within the same scope.
	rec = postJSON(t, s, base, `{"name":"ci"}`, h)
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate POST = %d, want %d", rec.Code, http.StatusConflict)
	}

	// The listing shows the stored record, never the raw value.
	rec = getWith(t, s, base, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after create = %d", rec.Code)
	}
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 || list[0].Name != "ci" || list[0].Org != "default" || list[0].Collection != "home" {
		t.Fatalf("list = %+v, want one scoped token named ci", list)
	}
	if strings.Contains(rec.Body.String(), created.Raw) {
		t.Error("listing leaked the raw token value")
	}

	// A token with the same name in another collection is a different scope.
	other := "/api/v1/orgs/lab/collections/edge/tokens"
	rec = postJSON(t, s, other, `{"name":"ci"}`, h)
	if rec.Code != http.StatusCreated {
		t.Errorf("POST %s = %d, want %d (body %s)", other, rec.Code, http.StatusCreated, rec.Body.String())
	}
	rec = getWith(t, s, other, h)
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil || len(list) != 1 || list[0].Collection != "edge" {
		t.Errorf("edge tokens = %+v (%v), want only the edge token", list, err)
	}

	// Revoke both scoped tokens.
	if rec := deleteWith(t, s, base+"/ci", h); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if rec := deleteWith(t, s, base+"/ci", h); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE again = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if rec := deleteWith(t, s, other+"/ci", h); rec.Code != http.StatusNoContent {
		t.Errorf("DELETE edge token = %d, want %d", rec.Code, http.StatusNoContent)
	}
	rec = getWith(t, s, base, h)
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil || len(list) != 0 {
		t.Errorf("list after remove = %+v (%v), want empty", list, err)
	}

	// Bad input and unknown collections are rejected.
	if rec := postJSON(t, s, base, `{not json`, h); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if rec := getWith(t, s, "/api/v1/orgs/nope/collections/home/tokens", h); rec.Code != http.StatusNotFound {
		t.Errorf("unknown collection = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestTokensRequireUser(t *testing.T) {
	d := testutil.OpenDB(t, "server")
	defer d.Close()
	path := "/api/v1/orgs/default/collections/home/tokens"

	// With users configured, unauthenticated requests are rejected by the
	// auth middleware before the handler runs.
	seedUser(t, d.Users(), "alice", "s3cret")
	s := newTokenTestServer(t, d)
	if rec := getWith(t, s, path, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET tokens anonymous = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	c := loginCookie(t, s, "alice", "s3cret")
	if rec := getWith(t, s, path, map[string]string{"Cookie": c.String()}); rec.Code != http.StatusOK {
		t.Errorf("GET tokens with session = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestTokenBearerIsScopedToItsCollection(t *testing.T) {
	d := testutil.OpenDB(t, "server")
	defer d.Close()
	seedUser(t, d.Users(), "alice", "s3cret")
	seedRunAt(t, d, "home", "site", "web1", time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	s := newTokenTestServer(t, d)

	raw, tok, err := d.Tokens().Create(context.Background(), "alice", "default", "home", "cli", 0)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if tok.Org != "default" || tok.Collection != "home" {
		t.Fatalf("stored token = %+v, want default/home scope", tok)
	}

	// The user token authenticates and the scope limits what can be read.
	h := map[string]string{"Authorization": "Bearer " + raw}
	if rec := getWith(t, s, "/api/v1/orgs/default/collections/home/hosts", h); rec.Code != http.StatusOK {
		t.Errorf("GET home hosts with bearer = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec := getWith(t, s, "/api/v1/orgs/default/collections/home/tokens", h); rec.Code != http.StatusOK {
		t.Errorf("GET home tokens with bearer = %d, want %d", rec.Code, http.StatusOK)
	}
	rec := getWith(t, s, "/api/v1/orgs", h)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET orgs with bearer = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var orgs []orgInfo
	if err := json.NewDecoder(rec.Body).Decode(&orgs); err != nil {
		t.Fatalf("decode orgs: %v", err)
	}
	if len(orgs) != 1 || orgs[0].Name != "default" || len(orgs[0].Collections) != 1 || orgs[0].Collections[0].Name != "home" {
		t.Errorf("visible orgs = %+v, want only default/home", orgs)
	}
	for _, path := range []string{
		"/api/v1/orgs/lab/collections/edge/hosts",
		"/api/v1/orgs/lab/collections/edge/tokens",
		"/api/v1/orgs/lab/collections/edge",
	} {
		if rec := getWith(t, s, path, h); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s with scoped bearer = %d, want %d", path, rec.Code, http.StatusForbidden)
		}
	}

	// The webhook-style header also carries user tokens.
	if rec := getWith(t, s, "/api/v1/orgs/default/collections/home/hosts", map[string]string{"X-Strangle-Token": raw}); rec.Code != http.StatusOK {
		t.Errorf("GET home hosts with X-Strangle-Token = %d, want %d", rec.Code, http.StatusOK)
	}

	// /me remains session-only: a bearer token is not a UI login.
	if rec := getWith(t, s, "/api/v1/me", h); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /me with bearer = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// Wrong and revoked values do not authenticate.
	if rec := getWith(t, s, "/api/v1/orgs", map[string]string{"Authorization": "Bearer wrong"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET orgs with wrong bearer = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if err := d.Tokens().Remove(context.Background(), "alice", "default", "home", "cli"); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}
	if rec := getWith(t, s, "/api/v1/orgs/default/collections/home/hosts", h); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET home hosts with revoked bearer = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestStaticTokenCannotManageTokens(t *testing.T) {
	d := testutil.OpenDB(t, "server")
	defer d.Close()
	seedUser(t, d.Users(), "alice", "s3cret")
	apr := applier.NewAnsible(testTargets, stubExecutor{}, d, zap.NewNop())
	s := api.New("test", apr, d, nil, nil, nil, d.Users(), d.Tokens(), "sekrit", zap.NewNop())

	path := "/api/v1/orgs/default/collections/home/tokens"
	for _, h := range []map[string]string{
		{"X-Strangle-Token": "sekrit"},
		{"Authorization": "Bearer sekrit"},
	} {
		if rec := getWith(t, s, path, h); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET tokens with static token = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if rec := postJSON(t, s, path, `{"name":"x"}`, h); rec.Code != http.StatusUnauthorized {
			t.Errorf("POST tokens with static token = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	}
}

func TestTokensNoUserIdentity(t *testing.T) {
	// No users (auth off) but a token store exists: the endpoints are
	// reachable, yet without a user identity they are rejected.
	d := testutil.OpenDB(t, "server")
	defer d.Close()
	s := newTokenTestServer(t, d)
	path := "/api/v1/orgs/default/collections/home/tokens"

	if rec := getWith(t, s, path, nil); rec.Code != http.StatusForbidden {
		t.Errorf("GET tokens anonymous = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if rec := postJSON(t, s, path, `{"name":"x"}`, nil); rec.Code != http.StatusForbidden {
		t.Errorf("POST tokens anonymous = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if rec := deleteWith(t, s, path+"/x", nil); rec.Code != http.StatusForbidden {
		t.Errorf("DELETE token anonymous = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestTokensWithoutStore(t *testing.T) {
	// No users, no tokens: auth is off, the endpoints report that token
	// storage is unavailable instead of panicking.
	s := newTestServer(t)
	path := "/api/v1/orgs/default/collections/home/tokens"

	if rec := getWith(t, s, path, nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET tokens without store = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if rec := postJSON(t, s, path, `{"name":"x"}`, nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("POST tokens without store = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if rec := deleteWith(t, s, path+"/x", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("DELETE token without store = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
