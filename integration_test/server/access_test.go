package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/auth"
	"github.com/kakwa/strangle/internal/db"
	"github.com/kakwa/strangle/internal/server"
	"github.com/kakwa/strangle/internal/testutil"
	"go.uber.org/zap"
)

// newAccessTestServer builds an authenticated server with two collections and
// the supplied membership store.
func newAccessTestServer(st db.Store, users auth.UserStore, tokens auth.TokenStore, access auth.CollectionAccess, opts ...server.Option) *server.Server {
	apr := applier.NewAnsible(orgTargets, stubExecutor{}, st, zap.NewNop())
	opts = append([]server.Option{server.WithCollectionAccess(access)}, opts...)
	return server.New("test", apr, st, nil, nil, nil, nil, users, tokens, "", zap.NewNop(), opts...)
}

func TestCollectionRestrictions(t *testing.T) {
	st := testutil.OpenDB(t, "server")
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	seedRunAt(t, st, "home", "site", "web1", base)
	seedRunAt(t, st, "edge", "site", "web1", base.Add(time.Hour))
	seedUser(t, st.Users(), "alice", "pw")
	access := st.Access()
	if err := access.Add(context.Background(), "alice", "lab", "edge"); err != nil {
		t.Fatalf("Add(): %v", err)
	}
	s := newAccessTestServer(st, st.Users(), st.Tokens(), access)
	cookie := loginCookie(t, s, "alice", "pw")
	h := map[string]string{"Cookie": cookie.String()}

	// Only the org/collection alice belongs to is listed.
	rec := getWith(t, s, "/api/v1/orgs", h)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET orgs: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var orgs []orgInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &orgs); err != nil {
		t.Fatalf("decode orgs: %v", err)
	}
	if len(orgs) != 1 || orgs[0].Name != "lab" || len(orgs[0].Collections) != 1 || orgs[0].Collections[0].Name != "edge" {
		t.Fatalf("visible orgs = %+v, want only lab/edge", orgs)
	}

	// Scoped resources follow the same membership rules.
	for _, tc := range []struct {
		path string
		code int
	}{
		{"/api/v1/orgs/default/collections/home", http.StatusForbidden},
		{"/api/v1/orgs/lab/collections/edge", http.StatusOK},
		{"/api/v1/orgs/default/collections/home/hosts", http.StatusForbidden},
		{"/api/v1/orgs/lab/collections/edge/hosts", http.StatusOK},
		{"/api/v1/orgs/default/collections/home/playbooks", http.StatusForbidden},
		{"/api/v1/orgs/lab/collections/edge/playbooks", http.StatusOK},
		{"/api/v1/orgs/default/collections/home/playbooks/site/hosts/web1/runs", http.StatusForbidden},
		{"/api/v1/orgs/lab/collections/edge/playbooks/site/hosts/web1/runs", http.StatusOK},
		{"/api/v1/orgs/default/collections/home/playbooks/site/hosts/web1/hostkeys", http.StatusForbidden},
	} {
		if rec := getWith(t, s, tc.path, h); rec.Code != tc.code {
			t.Errorf("GET %s: status = %d, want %d (body %s)", tc.path, rec.Code, tc.code, rec.Body.String())
		}
	}

	// Even a collection the user may see cannot expose another collection's
	// data: the data query is filtered by the collection in the URL.
	rec = getWith(t, s, "/api/v1/orgs/lab/collections/edge/hosts", h)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "home") {
		t.Errorf("edge hosts = %s, want only edge hosts", rec.Body.String())
	}
	rec = getWith(t, s, "/api/v1/orgs/lab/collections/edge/playbooks/site/hosts/web1/runs", h)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "home") {
		t.Errorf("edge host runs = %s, want only edge runs", rec.Body.String())
	}

	// Host-key decisions are scoped too. The access check runs before SSH
	// availability checks, so a foreign collection is denied with 403.
	patch := `[{"op":"replace","path":"/status","value":"approved"}]`
	foreign := "/api/v1/orgs/default/collections/home/playbooks/site/hosts/web1"
	if rec := patchJSON(t, s, foreign, patch, h); rec.Code != http.StatusForbidden {
		t.Errorf("PATCH foreign host = %d, want %d", rec.Code, http.StatusForbidden)
	}
	allowed := "/api/v1/orgs/lab/collections/edge/playbooks/site/hosts/web1"
	if rec := patchJSON(t, s, allowed, patch, h); rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH allowed host without SSH = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestScopedTokenCannotEscapeMemberships(t *testing.T) {
	st := testutil.OpenDB(t, "server")
	seedUser(t, st.Users(), "alice", "pw")
	access := st.Access()
	if err := access.Add(context.Background(), "alice", "lab", "edge"); err != nil {
		t.Fatalf("Add(): %v", err)
	}
	s := newAccessTestServer(st, st.Users(), st.Tokens(), access)

	homeRaw, _, err := st.Tokens().Create(context.Background(), "alice", "default", "home", "home-token", 0)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	edgeRaw, _, err := st.Tokens().Create(context.Background(), "alice", "lab", "edge", "edge-token", 0)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	homeH := map[string]string{"Authorization": "Bearer " + homeRaw}
	edgeH := map[string]string{"Authorization": "Bearer " + edgeRaw}

	// A token scoped to a collection the user is not a member of is useless,
	// and a valid scope cannot be used to reach a sibling collection.
	for _, tc := range []struct {
		path    string
		headers map[string]string
	}{
		{"/api/v1/orgs/default/collections/home/hosts", homeH},
		{"/api/v1/orgs/default/collections/home/hosts", edgeH},
		{"/api/v1/orgs/lab/collections/edge/hosts", homeH},
	} {
		if rec := getWith(t, s, tc.path, tc.headers); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s with scoped token: status = %d, want %d (body %s)", tc.path, rec.Code, http.StatusForbidden, rec.Body.String())
		}
	}
	if rec := getWith(t, s, "/api/v1/orgs/lab/collections/edge/hosts", edgeH); rec.Code != http.StatusOK {
		t.Errorf("GET edge hosts with edge token = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestNoMembershipsMeansUnrestricted(t *testing.T) {
	st := testutil.OpenDB(t, "server")
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	seedRunAt(t, st, "home", "site", "web1", base)
	seedRunAt(t, st, "edge", "site", "web1", base.Add(time.Hour))
	seedUser(t, st.Users(), "bob", "pw")
	s := newAccessTestServer(st, st.Users(), st.Tokens(), st.Access())
	cookie := loginCookie(t, s, "bob", "pw")
	h := map[string]string{"Cookie": cookie.String()}

	rec := getWith(t, s, "/api/v1/orgs", h)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET orgs: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var orgs []orgInfo
	if err := json.NewDecoder(rec.Body).Decode(&orgs); err != nil {
		t.Fatalf("decode orgs: %v", err)
	}
	if len(orgs) != 2 {
		t.Errorf("orgs = %+v, want both collections while no membership is configured", orgs)
	}
	for _, path := range []string{
		"/api/v1/orgs/default/collections/home/hosts",
		"/api/v1/orgs/lab/collections/edge/hosts",
	} {
		if rec := getWith(t, s, path, h); rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want %d", path, rec.Code, http.StatusOK)
		}
	}
}
