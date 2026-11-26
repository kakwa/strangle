package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/kakwa/strangle/cfg"
	"github.com/kakwa/strangle/controller/applier"
	"github.com/kakwa/strangle/security/auth"
	"github.com/kakwa/strangle/tests/testutil"
	swag "github.com/swaggo/swag/v2"
	"go.uber.org/zap"
)

// Minimal view of the generated OpenAPI document: enough to check that swag
// produced a complete 3.1 spec without coupling the test to generated Go
// structs.
type openAPIDoc struct {
	OpenAPI string `json:"openapi"`
	Info    struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	Paths map[string]map[string]struct {
		OperationID string                     `json:"operationId"`
		Responses   map[string]json.RawMessage `json:"responses"`
		RequestBody *struct {
			Content map[string]struct {
				Schema struct {
					Ref string `json:"$ref"`
				} `json:"schema"`
			} `json:"content"`
		} `json:"requestBody"`
	} `json:"paths"`
	Components struct {
		SecuritySchemes map[string]json.RawMessage `json:"securitySchemes"`
		Schemas         map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"schemas"`
	} `json:"components"`
}

var echoParamPattern = regexp.MustCompile(`:([\w]+)`)

type openAPINoEnqueuer struct{}

func (openAPINoEnqueuer) EnqueueRun(context.Context, string, string, uint64, bool) (string, error) {
	return "1", nil
}

type openAPIExecutor struct{}

func (openAPIExecutor) RunPlaybook(context.Context, string, string, string, ...string) (int, string, map[string]json.RawMessage, error) {
	return 0, "", nil, nil
}

// newOpenAPITestServer builds a server with no store: the documentation
// routes under test never read one.
func newOpenAPITestServer(users auth.UserStore) *Server {
	targets := []cfg.TargetConfig{{
		Name: "home",
		Playbooks: []cfg.PlaybookConfig{
			{Name: "site", Playbook: "site.yml", Inventory: "inventory.ini"},
		},
	}}
	apr := applier.NewAnsible(targets, openAPIExecutor{}, nil, zap.NewNop())
	return New("test", apr, nil, nil, openAPINoEnqueuer{}, nil, users, nil, "", zap.NewNop())
}

func openAPIGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func generatedSpec(t *testing.T) openAPIDoc {
	t.Helper()
	raw, err := swag.ReadDoc()
	if err != nil {
		t.Fatalf("swag.ReadDoc(): %v", err)
	}
	var spec openAPIDoc
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatalf("decode generated spec: %v", err)
	}
	return spec
}

func TestOpenAPISpecCoversVersionedRoutes(t *testing.T) {
	s := newOpenAPITestServer(nil)
	e := s.buildRoutes()
	spec := generatedSpec(t)

	if !strings.HasPrefix(spec.OpenAPI, "3.1") {
		t.Errorf("openapi = %q, want 3.1.x", spec.OpenAPI)
	}
	if spec.Info.Title != "Strangle API" || spec.Info.Version != "v1" {
		t.Errorf("info = %+v, want Strangle API v1", spec.Info)
	}
	if len(spec.Paths) < 18 {
		t.Fatalf("spec has %d paths, want the whole versioned API", len(spec.Paths))
	}

	// Every versioned Echo route must appear in the generated document, and
	// every documented operation must be registered on the router.
	registered := map[string]bool{}
	for _, route := range e.Router().Routes() {
		path := echoParamPattern.ReplaceAllString(route.Path, "{$1}")
		method := strings.ToLower(route.Method)
		registered[method+" "+path] = true
		if !strings.HasPrefix(route.Path, apiPrefix+"/") {
			continue
		}
		ops, ok := spec.Paths[path]
		if !ok {
			t.Errorf("path %q is registered but missing from the spec", route.Path)
			continue
		}
		if _, ok := ops[method]; !ok {
			t.Errorf("%s %s is registered but missing from the spec", method, path)
		}
	}
	for path, ops := range spec.Paths {
		for method := range ops {
			if !registered[method+" "+path] {
				t.Errorf("%s %s is documented but not registered on the router", method, path)
			}
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

	hook := spec.Paths[apiPrefix+"/orgs/{org}/collections/{collection}/hook"]["post"]
	if hook.RequestBody == nil {
		t.Fatal("collection hook has no request body in the spec")
	}
	media, ok := hook.RequestBody.Content[jsonMedia]
	if !ok || !strings.Contains(media.Schema.Ref, "syncRequest") {
		t.Fatalf("hook request schema = %+v, want syncRequest", media.Schema)
	}
	name := strings.TrimPrefix(media.Schema.Ref, "#/components/schemas/")
	schema, ok := spec.Components.Schemas[name]
	if !ok {
		t.Fatalf("hook request schema %q is missing from components", name)
	}
	if _, ok := schema.Properties["branch"]; !ok {
		t.Error("hook request schema has no \"branch\" property")
	}
	if _, ok := spec.Components.SecuritySchemes["userBearer"]; !ok {
		t.Error("spec does not define the user token security scheme")
	}
}

func TestOpenAPIEndpoints(t *testing.T) {
	s := newOpenAPITestServer(nil)

	rec := openAPIGet(t, s, "/api/v1/openapi.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("spec status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != jsonMedia {
		t.Errorf("spec content type = %q, want %q", got, jsonMedia)
	}
	var spec openAPIDoc
	if err := json.NewDecoder(rec.Body).Decode(&spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if len(spec.Paths) == 0 {
		t.Error("served spec has no paths")
	}

	rec = openAPIGet(t, s, "/api/docs")
	if rec.Code != http.StatusOK {
		t.Fatalf("docs status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, htmlMedia) {
		t.Errorf("docs content type = %q, want HTML", got)
	}
	if !strings.Contains(rec.Body.String(), "SwaggerUIBundle") {
		t.Error("docs page does not initialize Swagger UI")
	}
	// html/template escapes slashes inside the embedded JS string.
	if !strings.Contains(rec.Body.String(), "api\\/v1\\/openapi.json") {
		t.Error("docs page does not point at the versioned spec")
	}
}

func TestOpenAPIDocsArePublicWhenAuthOn(t *testing.T) {
	d := testutil.OpenDB(t, "openapi")
	defer d.Close()
	users := d.Users()
	hash, err := auth.HashPassword("s3cret")
	if err != nil {
		t.Fatalf("HashPassword(): %v", err)
	}
	if err := users.CreateUser(context.Background(), "alice", hash); err != nil {
		t.Fatalf("CreateUser(): %v", err)
	}
	s := newOpenAPITestServer(users)

	for _, path := range []string{"/api/docs", "/api/docs/swagger-ui.css", "/api/v1/openapi.json"} {
		if rec := openAPIGet(t, s, path); rec.Code != http.StatusOK {
			t.Errorf("%s without a session = %d, want %d", path, rec.Code, http.StatusOK)
		}
	}
	if rec := openAPIGet(t, s, "/api/v1/orgs"); rec.Code != http.StatusUnauthorized {
		t.Errorf("/api/v1/orgs without a session = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
