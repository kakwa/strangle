package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kakwa/strangle/internal/auth"
)

func TestOpenAPISpecCoversVersionedRoutes(t *testing.T) {
	s := newTestServer(&recEnqueuer{status: "queued"})
	_, catalog := s.buildRoutes()
	spec := buildOpenAPISpec(catalog)

	if spec.OpenAPI != "3.0.3" {
		t.Errorf("openapi = %q, want 3.0.3", spec.OpenAPI)
	}
	if len(spec.Paths) < 30 {
		t.Fatalf("spec has %d paths, want the whole versioned API", len(spec.Paths))
	}

	// Every versioned pattern the mux registered must appear in the spec.
	for _, pattern := range catalog.patterns {
		method, path, ok := splitPattern(pattern)
		if !ok || !strings.HasPrefix(path, apiPrefix+"/") {
			continue
		}
		ops, ok := spec.Paths[path]
		if !ok {
			t.Errorf("path %q is registered but missing from the spec", path)
			continue
		}
		if _, ok := ops[method]; !ok {
			t.Errorf("%s %s is registered but missing from the spec", method, path)
		}
	}

	// Legacy flat aliases stay out of the document; /api/docs is intentional.
	operationIDs := map[string]string{}
	for path, ops := range spec.Paths {
		if strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, apiPrefix+"/") && path != "/api/docs" {
			t.Errorf("spec documents legacy path %q", path)
		}
		for method, op := range ops {
			if op.OperationID == "" {
				t.Errorf("%s %s has no operationId", method, path)
				continue
			}
			if previous, duplicate := operationIDs[op.OperationID]; duplicate {
				t.Errorf("operationId %q is used by %s and %s %s", op.OperationID, previous, method, path)
			}
			operationIDs[op.OperationID] = method + " " + path
			if len(op.Responses) == 0 {
				t.Errorf("%s %s has no documented responses", method, path)
			}
		}
	}

	run := spec.Paths[apiPrefix+"/orgs/{org}/collections/{collection}/run"]["POST"]
	if run.RequestBody == nil {
		t.Fatal("collection run has no request body in the spec")
	} else {
		props := run.RequestBody.Content[jsonMedia].Schema.Properties
		if _, ok := props["playbook"]; !ok {
			t.Error("run request schema has no playbook property")
		}
		if _, ok := props["check"]; !ok {
			t.Error("run request schema has no check property")
		}
	}
	runs := spec.Paths[apiPrefix+"/runs"]["GET"]
	if _, ok := runs.Responses["200"].Headers["X-Total-Count"]; !ok {
		t.Error("run list response does not document X-Total-Count")
	}
	if _, ok := spec.Components.SecuritySchemes["userBearer"]; !ok {
		t.Error("spec does not define the user token security scheme")
	}
}

func TestOpenAPIEndpoints(t *testing.T) {
	s := newTestServer(&recEnqueuer{status: "queued"})

	rec := doGet(t, s, "/api/v1/openapi.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("spec status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != jsonMedia {
		t.Errorf("spec content type = %q, want %q", got, jsonMedia)
	}
	var spec openAPISpec
	if err := json.NewDecoder(rec.Body).Decode(&spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if len(spec.Paths) == 0 {
		t.Error("served spec has no paths")
	}

	rec = doGet(t, s, "/api/docs")
	if rec.Code != http.StatusOK {
		t.Fatalf("docs status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, htmlMedia) {
		t.Errorf("docs content type = %q, want HTML", got)
	}
	if !strings.Contains(rec.Body.String(), "SwaggerUIBundle") {
		t.Error("docs page does not initialize Swagger UI")
	}
	if !strings.Contains(rec.Body.String(), "/api/v1/openapi.json") {
		t.Error("docs page does not point at the versioned spec")
	}
}

func TestOpenAPIDocsArePublicWhenAuthOn(t *testing.T) {
	users := auth.NewMemUserStore()
	seedUser(t, users, "alice", "s3cret")
	s := newAuthTestServer(users)

	for _, path := range []string{"/api/docs", "/api/v1/openapi.json"} {
		if rec := doGet(t, s, path); rec.Code != http.StatusOK {
			t.Errorf("%s without a session = %d, want %d", path, rec.Code, http.StatusOK)
		}
	}
	if rec := doGet(t, s, "/api/stats"); rec.Code != http.StatusUnauthorized {
		t.Errorf("/api/stats without a session = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
