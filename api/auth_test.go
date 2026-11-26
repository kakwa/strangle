package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kakwa/strangle/security/auth"
	"github.com/labstack/echo/v5"
)

type stubCollectionAccess struct{ allowed map[string]bool }

func (s stubCollectionAccess) Add(context.Context, string, string, string) error { return nil }
func (s stubCollectionAccess) Remove(context.Context, string, string, string) error {
	return nil
}
func (s stubCollectionAccess) List(context.Context, string) ([]auth.CollectionMembership, error) {
	return nil, nil
}
func (s stubCollectionAccess) HasAny(context.Context) (bool, error) { return len(s.allowed) > 0, nil }
func (s stubCollectionAccess) Allows(_ context.Context, _, org, collection string) (bool, error) {
	return s.allowed[org+"/"+collection], nil
}
func (s stubCollectionAccess) Close() error { return nil }

func TestCollectionAccessMiddleware(t *testing.T) {
	api := &API{
		AuthOn: true,
		Access: stubCollectionAccess{allowed: map[string]bool{"lab/edge": true}},
	}

	e := echo.New()
	e.HTTPErrorHandler = apiErrorHandler
	e.Use(collectionAccessMiddleware(api))
	called := false
	ok := func(c *echo.Context) error {
		called = true
		return c.NoContent(http.StatusOK)
	}
	e.GET("/api/v1/orgs/:org/collections/:collection", ok)
	e.GET("/api/v1/orgs", ok)

	cases := []struct {
		name     string
		path     string
		identity Auth
		wantCode int
	}{
		{"member", "/api/v1/orgs/lab/collections/edge", Auth{User: "alice"}, http.StatusOK},
		{"foreign collection", "/api/v1/orgs/default/collections/home", Auth{User: "alice"}, http.StatusForbidden},
		{"token scope mismatch", "/api/v1/orgs/lab/collections/edge", Auth{
			User:        "alice",
			TokenScopes: []TokenScope{{Org: "default", Collection: "home"}},
		}, http.StatusForbidden},
		{"no collection parameter", "/api/v1/orgs", Auth{User: "alice"}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r = r.WithContext(WithAuth(r.Context(), tc.identity))
			w := httptest.NewRecorder()
			e.ServeHTTP(w, r)

			if w.Code != tc.wantCode {
				t.Errorf("status = %d, want %d (body %s)", w.Code, tc.wantCode, w.Body.String())
			}
			if called != (tc.wantCode == http.StatusOK) {
				t.Errorf("handler called = %v, want %v", called, tc.wantCode == http.StatusOK)
			}
		})
	}
}
