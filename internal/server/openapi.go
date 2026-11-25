package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/kakwa/strangle/internal/auth"
	"github.com/kakwa/strangle/internal/sshpool"
	"github.com/kakwa/strangle/internal/store"
	"github.com/kakwa/strangle/internal/worker"
)

// routeCatalog records every registered mux pattern so the OpenAPI document
// is generated from the same routes the server actually serves.
type routeCatalog struct {
	mux      *http.ServeMux
	patterns []string
}

func newRouteCatalog() *routeCatalog {
	return &routeCatalog{mux: http.NewServeMux()}
}

func (c *routeCatalog) HandleFunc(pattern string, handler http.HandlerFunc) {
	c.patterns = append(c.patterns, pattern)
	c.mux.HandleFunc(pattern, handler)
}

func (c *routeCatalog) Handle(pattern string, handler http.Handler) {
	c.patterns = append(c.patterns, pattern)
	c.mux.Handle(pattern, handler)
}

func (c *routeCatalog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mux.ServeHTTP(w, r)
}

// OpenAPI document types. Only the fields strangle needs are modelled; the
// schema walker below fills in request/response structures from Go types.
type openAPISpec struct {
	OpenAPI    string                          `json:"openapi"`
	Info       openAPIInfo                     `json:"info"`
	Paths      map[string]map[string]openAPIOp `json:"paths"`
	Components openAPIComponents               `json:"components"`
}

type openAPIInfo struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

type openAPIComponents struct {
	SecuritySchemes map[string]openAPISecurityScheme `json:"securitySchemes"`
}

type openAPISecurityScheme struct {
	Type         string `json:"type"`
	Description  string `json:"description,omitempty"`
	Name         string `json:"name,omitempty"`
	In           string `json:"in,omitempty"`
	Scheme       string `json:"scheme,omitempty"`
	BearerFormat string `json:"bearerFormat,omitempty"`
}

type openAPIOp struct {
	OperationID string                     `json:"operationId"`
	Summary     string                     `json:"summary"`
	Tags        []string                   `json:"tags"`
	Deprecated  bool                       `json:"deprecated,omitempty"`
	Security    []map[string][]string      `json:"security,omitempty"`
	Parameters  []openAPIParam             `json:"parameters,omitempty"`
	RequestBody *openAPIRequestBody        `json:"requestBody,omitempty"`
	Responses   map[string]openAPIResponse `json:"responses"`
}

type openAPIParam struct {
	Name        string        `json:"name"`
	In          string        `json:"in"`
	Description string        `json:"description,omitempty"`
	Required    bool          `json:"required"`
	Schema      openAPISchema `json:"schema"`
}

type openAPIRequestBody struct {
	Required bool                        `json:"required"`
	Content  map[string]openAPIMediaType `json:"content"`
}

type openAPIMediaType struct {
	Schema openAPISchema `json:"schema"`
}

type openAPIResponse struct {
	Description string                      `json:"description"`
	Headers     map[string]openAPIHeader    `json:"headers,omitempty"`
	Content     map[string]openAPIMediaType `json:"content,omitempty"`
}

type openAPIHeader struct {
	Description string        `json:"description,omitempty"`
	Schema      openAPISchema `json:"schema"`
}

type openAPISchema struct {
	Type                 string                   `json:"type,omitempty"`
	Format               string                   `json:"format,omitempty"`
	Description          string                   `json:"description,omitempty"`
	Default              any                      `json:"default,omitempty"`
	Properties           map[string]openAPISchema `json:"properties,omitempty"`
	Required             []string                 `json:"required,omitempty"`
	Items                *openAPISchema           `json:"items,omitempty"`
	AdditionalProperties *openAPISchema           `json:"additionalProperties,omitempty"`
}

const (
	docAuthOpen = "open"
	docAuthUser = "user"
	docAuthSync = "sync"
	jsonMedia   = "application/json"
	htmlMedia   = "text/html"
)

// Named shapes used by the generated schemas. They mirror the anonymous
// structs decoded/written by the handlers so the spec documents real bodies.
type loginRequestDoc struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type tokenCreateRequestDoc struct {
	Name     string `json:"name"`
	TTLHours int64  `json:"ttl_hours,omitempty"`
}

type tokenCreateResponseDoc struct {
	Raw   string     `json:"raw"`
	Token auth.Token `json:"token"`
}

type runRequestDoc struct {
	Target   string `json:"target"`
	Playbook string `json:"playbook"`
	Check    bool   `json:"check,omitempty"`
}

type runAcceptedResponseDoc struct {
	Status   string `json:"status"`
	Target   string `json:"target"`
	Playbook string `json:"playbook"`
	RunID    uint64 `json:"run_id"`
	Check    bool   `json:"check"`
}

type syncRequestDoc struct {
	Target string `json:"target"`
	Branch string `json:"branch,omitempty"`
}

type syncResponseDoc struct {
	Target  string `json:"target"`
	Rev     string `json:"rev"`
	Changed bool   `json:"changed"`
}

type cancelRunResponseDoc struct {
	ID    uint64 `json:"id"`
	State string `json:"state"`
}

type hostKeyDecisionRequestDoc struct {
	Target      string `json:"target"`
	Host        string `json:"host"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type hostKeyDecisionResponseDoc struct {
	Status string `json:"status"`
	Host   string `json:"host"`
}

type messageResponseDoc struct {
	Status string `json:"status"`
}

type meResponseDoc struct {
	Authenticated bool   `json:"authenticated"`
	Username      string `json:"username,omitempty"`
}

type collectionDetailResponseDoc struct {
	Name      string            `json:"name"`
	Org       string            `json:"org"`
	Playbooks []playbookInfo    `json:"playbooks"`
	Links     map[string]string `json:"links"`
}

type apiErrorDoc struct {
	Error string `json:"error"`
}

type queryParamDef struct {
	name        string
	description string
	kind        reflect.Type
	def         any
}

func query(name, description string, kind any, def any) queryParamDef {
	return queryParamDef{name: name, description: description, kind: reflect.TypeOf(kind), def: def}
}

var (
	runsQueries = []queryParamDef{
		query("playbook", "Only runs of this playbook", "", nil),
		query("host", "Only runs that mention this host", "", nil),
		query("from", "Inclusive RFC3339 start time", time.Time{}, nil),
		query("to", "Exclusive RFC3339 end time", time.Time{}, nil),
		query("limit", "Maximum number of runs (default 50)", 0, 50),
		query("offset", "Number of matching runs to skip", 0, 0),
	}
	limit10  = []queryParamDef{query("limit", "Maximum number of entries (default 10)", 0, 10)}
	limit200 = []queryParamDef{query("limit", "Maximum number of entries (default 200)", 0, 200)}
)

// responseMeta describes one documented response.
type responseMeta struct {
	description string
	body        any
	contentType string
	headers     map[string]string
}

func result(code int, description string, body any) responseMeta {
	return responseMeta{description: description, body: body}
}

func withErrors(responses map[int]responseMeta, codes ...int) map[int]responseMeta {
	for _, code := range codes {
		responses[code] = result(code, http.StatusText(code), apiErrorDoc{})
	}
	return responses
}

type operationMeta struct {
	summary   string
	tags      []string
	auth      string
	queries   []queryParamDef
	request   any
	responses map[int]responseMeta
}

// operationMetadata is keyed by "METHOD /api/v1/...". Flat legacy aliases are
// intentionally absent: the document describes the versioned API only.
var operationMetadata = map[string]operationMeta{
	"GET " + apiPrefix + "/health": {
		summary:   "Liveness probe",
		tags:      []string{"system"},
		auth:      docAuthOpen,
		responses: map[int]responseMeta{200: result(200, "Server is up", messageResponseDoc{})},
	},
	"POST " + apiPrefix + "/login": {
		summary: "Log in and start a session",
		tags:    []string{"auth"},
		auth:    docAuthOpen,
		request: loginRequestDoc{},
		responses: withErrors(map[int]responseMeta{
			200: result(200, "Authenticated; sets the strangle_session cookie", meResponseDoc{}),
		}, 400, 401),
	},
	"POST " + apiPrefix + "/logout": {
		summary:   "Clear the session cookie",
		tags:      []string{"auth"},
		auth:      docAuthOpen,
		responses: map[int]responseMeta{200: result(200, "Logged out", messageResponseDoc{})},
	},
	"GET " + apiPrefix + "/me": {
		summary:   "Current session identity",
		tags:      []string{"auth"},
		auth:      docAuthOpen,
		responses: withErrors(map[int]responseMeta{200: result(200, "Identity (or unauthenticated)", meResponseDoc{})}, 401),
	},
	"GET " + apiPrefix + "/tokens": {
		summary:   "List personal access tokens",
		tags:      []string{"auth"},
		auth:      docAuthUser,
		responses: withErrors(map[int]responseMeta{200: result(200, "Tokens", []auth.Token{})}, 401, 403, 503),
	},
	"POST " + apiPrefix + "/tokens": {
		summary: "Create a personal access token",
		tags:    []string{"auth"},
		auth:    docAuthUser,
		request: tokenCreateRequestDoc{},
		responses: withErrors(map[int]responseMeta{
			201: result(201, "Created; raw value is shown once", tokenCreateResponseDoc{}),
		}, 400, 401, 403, 409, 503),
	},
	"DELETE " + apiPrefix + "/tokens/{name}": {
		summary:   "Revoke a personal access token",
		tags:      []string{"auth"},
		auth:      docAuthUser,
		responses: withErrors(map[int]responseMeta{204: result(204, "Revoked", nil)}, 401, 403, 404, 500, 503),
	},
	"GET " + apiPrefix + "/stats": {
		summary:   "Aggregate statistics for visible collections",
		tags:      []string{"stats"},
		auth:      docAuthUser,
		responses: withErrors(map[int]responseMeta{200: result(200, "Statistics", store.Stats{})}, 401, 500),
	},
	"GET " + apiPrefix + "/runs": {
		summary:   "List runs without captured output",
		tags:      []string{"runs"},
		auth:      docAuthUser,
		queries:   runsQueries,
		responses: withErrors(runListResponses(), 400, 401, 500),
	},
	"GET " + apiPrefix + "/runs/{id}": {
		summary:   "Run details including captured output",
		tags:      []string{"runs"},
		auth:      docAuthUser,
		responses: withErrors(map[int]responseMeta{200: result(200, "Run", store.RunRecord{})}, 400, 401, 404, 500),
	},
	"GET " + apiPrefix + "/hosts": {
		summary:   "List known hosts",
		tags:      []string{"hosts"},
		auth:      docAuthUser,
		responses: withErrors(map[int]responseMeta{200: result(200, "Hosts", []store.HostRecord{})}, 401, 500),
	},
	"GET " + apiPrefix + "/hosts/{host}/runs": {
		summary:   "Run history of one host",
		tags:      []string{"hosts"},
		auth:      docAuthUser,
		queries:   limit10,
		responses: withErrors(map[int]responseMeta{200: result(200, "Host runs", []store.HostRunRecord{})}, 401, 500),
	},
	"GET " + apiPrefix + "/hosts/{host}/changes": {
		summary:   "Latest changes of one host",
		tags:      []string{"hosts"},
		auth:      docAuthUser,
		queries:   limit200,
		responses: withErrors(map[int]responseMeta{200: result(200, "Task changes", []store.TaskStateRecord{})}, 401, 500),
	},
	"GET " + apiPrefix + "/playbooks": {
		summary:   "Configured playbooks grouped by collection",
		tags:      []string{"playbooks"},
		auth:      docAuthUser,
		responses: withErrors(map[int]responseMeta{200: result(200, "Playbooks", map[string][]playbookInfo{})}, 401, 500),
	},
	"GET " + apiPrefix + "/workers": {
		summary:   "Worker pool status",
		tags:      []string{"workers"},
		auth:      docAuthUser,
		responses: withErrors(map[int]responseMeta{200: result(200, "Workers", []worker.WorkerStatus{})}, 401),
	},
	"POST " + apiPrefix + "/run": {
		summary:   "Queue a playbook run (legacy alias for collection runs)",
		tags:      []string{"runs"},
		auth:      docAuthUser,
		request:   runRequestDoc{},
		responses: withErrors(map[int]responseMeta{202: result(202, "Queued", runAcceptedResponseDoc{})}, 400, 401, 403, 404, 500),
	},
	"POST " + apiPrefix + "/sync": {
		summary: "Synchronize a git-backed collection",
		tags:    []string{"collections"},
		auth:    docAuthSync,
		request: syncRequestDoc{},
		responses: withErrors(map[int]responseMeta{
			200: result(200, "Synchronized", syncResponseDoc{}),
		}, 400, 401, 403, 404, 502),
	},
	"GET " + apiPrefix + "/orgs": {
		summary:   "List visible organizations and their collections",
		tags:      []string{"collections"},
		auth:      docAuthUser,
		responses: withErrors(map[int]responseMeta{200: result(200, "Organizations", []orgInfo{})}, 401),
	},
	"GET " + apiPrefix + "/orgs/{org}": {
		summary:   "Describe one organization",
		tags:      []string{"collections"},
		auth:      docAuthUser,
		responses: withErrors(map[int]responseMeta{200: result(200, "Organization", orgInfo{})}, 401, 404),
	},
	"GET " + apiPrefix + "/orgs/{org}/collections": {
		summary:   "List the collections of an organization",
		tags:      []string{"collections"},
		auth:      docAuthUser,
		responses: withErrors(map[int]responseMeta{200: result(200, "Collections", []collectionInfo{})}, 401, 404),
	},
	"GET " + apiPrefix + "/orgs/{org}/collections/{collection}": {
		summary:   "Describe one collection and its sub-resource links",
		tags:      []string{"collections"},
		auth:      docAuthUser,
		responses: withErrors(map[int]responseMeta{200: result(200, "Collection", collectionDetailResponseDoc{})}, 401, 403, 404),
	},
}

func init() {
	base := apiPrefix + "/orgs/{org}/collections/{collection}"
	scoped := map[string]operationMeta{
		"GET " + base + "/stats": {
			summary:   "Statistics for this collection",
			tags:      []string{"stats"},
			auth:      docAuthUser,
			responses: withErrors(map[int]responseMeta{200: result(200, "Statistics", store.Stats{})}, 401, 403, 404, 500),
		},
		"GET " + base + "/runs": {
			summary:   "List runs of this collection",
			tags:      []string{"runs"},
			auth:      docAuthUser,
			queries:   runsQueries,
			responses: withErrors(runListResponses(), 400, 401, 403, 404, 500),
		},
		"GET " + base + "/runs/{id}": {
			summary:   "Run details for this collection",
			tags:      []string{"runs"},
			auth:      docAuthUser,
			responses: withErrors(map[int]responseMeta{200: result(200, "Run", store.RunRecord{})}, 400, 401, 403, 404, 500),
		},
		"GET " + base + "/runs/{id}/tasks": {
			summary:   "Per-task history of a run",
			tags:      []string{"runs"},
			auth:      docAuthUser,
			responses: withErrors(map[int]responseMeta{200: result(200, "Task runs", []store.TaskRunRecord{})}, 400, 401, 403, 404, 500),
		},
		"GET " + base + "/hosts": {
			summary:   "Hosts of this collection",
			tags:      []string{"hosts"},
			auth:      docAuthUser,
			responses: withErrors(map[int]responseMeta{200: result(200, "Hosts", []store.HostRecord{})}, 401, 403, 404, 500),
		},
		"GET " + base + "/hosts/{host}/runs": {
			summary:   "Run history of one host in this collection",
			tags:      []string{"hosts"},
			auth:      docAuthUser,
			queries:   limit10,
			responses: withErrors(map[int]responseMeta{200: result(200, "Host runs", []store.HostRunRecord{})}, 401, 403, 404, 500),
		},
		"GET " + base + "/hosts/{host}/changes": {
			summary:   "Latest changes of one host in this collection",
			tags:      []string{"hosts"},
			auth:      docAuthUser,
			queries:   limit200,
			responses: withErrors(map[int]responseMeta{200: result(200, "Task changes", []store.TaskStateRecord{})}, 401, 403, 404, 500),
		},
		"GET " + base + "/playbooks": {
			summary:   "Playbooks of this collection",
			tags:      []string{"playbooks"},
			auth:      docAuthUser,
			responses: withErrors(map[int]responseMeta{200: result(200, "Playbooks", map[string][]playbookInfo{})}, 401, 403, 404, 500),
		},
		"POST " + base + "/run": {
			summary:   "Queue a playbook run in this collection",
			tags:      []string{"runs"},
			auth:      docAuthUser,
			request:   runRequestDoc{},
			responses: withErrors(map[int]responseMeta{202: result(202, "Queued", runAcceptedResponseDoc{})}, 400, 401, 403, 404, 500),
		},
		"POST " + base + "/runs/{id}/cancel": {
			summary:   "Cancel a queued or running job",
			tags:      []string{"runs"},
			auth:      docAuthUser,
			responses: withErrors(map[int]responseMeta{200: result(200, "Cancellation requested", cancelRunResponseDoc{})}, 400, 401, 403, 404, 409, 500, 501),
		},
		"POST " + base + "/sync": {
			summary: "Synchronize this collection",
			tags:    []string{"collections"},
			auth:    docAuthSync,
			request: syncRequestDoc{},
			responses: withErrors(map[int]responseMeta{
				200: result(200, "Synchronized", syncResponseDoc{}),
			}, 400, 401, 403, 404, 502),
		},
		"GET " + base + "/hostkeys": {
			summary:   "Pending SSH host keys of this collection",
			tags:      []string{"ssh"},
			auth:      docAuthUser,
			responses: withErrors(map[int]responseMeta{200: result(200, "Host keys", []sshpool.HostKey{})}, 401, 403, 404),
		},
		"GET " + base + "/ssh/stats": {
			summary:   "SSH connection pool counters",
			tags:      []string{"ssh"},
			auth:      docAuthUser,
			responses: withErrors(map[int]responseMeta{200: result(200, "Pool stats", map[string]sshpool.Stats{})}, 401, 403, 404),
		},
		"POST " + base + "/hostkeys/accept": {
			summary: "Approve a pending SSH host key",
			tags:    []string{"ssh"},
			auth:    docAuthUser,
			request: hostKeyDecisionRequestDoc{},
			responses: withErrors(map[int]responseMeta{
				200: result(200, "Approved", hostKeyDecisionResponseDoc{}),
			}, 400, 401, 403, 404),
		},
		"POST " + base + "/hostkeys/reject": {
			summary: "Reject a pending SSH host key",
			tags:    []string{"ssh"},
			auth:    docAuthUser,
			request: hostKeyDecisionRequestDoc{},
			responses: withErrors(map[int]responseMeta{
				200: result(200, "Rejected", hostKeyDecisionResponseDoc{}),
			}, 400, 401, 403, 404),
		},
		"GET " + apiPrefix + "/openapi.json": {
			summary:   "OpenAPI 3 description of the versioned API",
			tags:      []string{"system"},
			auth:      docAuthOpen,
			responses: map[int]responseMeta{200: result(200, "OpenAPI document", map[string]any{})},
		},
		"GET /api/docs": {
			summary:   "Swagger UI for the versioned API",
			tags:      []string{"system"},
			auth:      docAuthOpen,
			responses: map[int]responseMeta{200: {description: "Swagger UI", contentType: htmlMedia, body: ""}},
		},
	}
	for key, meta := range scoped {
		operationMetadata[key] = meta
	}
}

func runListResponses() map[int]responseMeta {
	r := result(200, "Runs", []store.RunRecord{})
	r.headers = map[string]string{"X-Total-Count": "Total matching runs before pagination"}
	return map[int]responseMeta{200: r}
}

// registerDocsRoutes serves the generated spec and a small Swagger UI page.
func (s *Server) registerDocsRoutes(mux *routeCatalog) {
	// The spec itself lives under the versioned prefix; only the human-facing
	// Swagger UI uses the conventional /api/docs path from the feature list.
	mux.HandleFunc("GET "+apiPrefix+"/openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, buildOpenAPISpec(mux))
	})
	mux.HandleFunc("GET /api/docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", htmlMedia+"; charset=utf-8")
		_, _ = w.Write([]byte(swaggerHTML))
	})
}

const swaggerHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Strangle API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <p><a href="/api/v1/openapi.json">OpenAPI JSON</a></p>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    window.onload = function () {
      SwaggerUIBundle({ url: "/api/v1/openapi.json", dom_id: "#swagger-ui" });
    };
  </script>
</body>
</html>
`

var pathParamPattern = regexp.MustCompile(`\{([^}]+)\}`)

func buildOpenAPISpec(catalog *routeCatalog) openAPISpec {
	spec := openAPISpec{
		OpenAPI: "3.0.3",
		Info: openAPIInfo{
			Title:       "Strangle API",
			Version:     apiVersion,
			Description: "Generated from the routes registered by the server. Legacy flat /api/* aliases are intentionally omitted.",
		},
		Paths: map[string]map[string]openAPIOp{},
		Components: openAPIComponents{SecuritySchemes: map[string]openAPISecurityScheme{
			"sessionCookie": {
				Type:        "apiKey",
				In:          "cookie",
				Name:        "strangle_session",
				Description: "Session cookie returned by POST /api/v1/login.",
			},
			"userBearer": {
				Type:         "http",
				Scheme:       "bearer",
				BearerFormat: "st_ personal access token",
				Description:  "User token created through POST /api/v1/tokens.",
			},
			"syncToken": {
				Type:        "apiKey",
				In:          "header",
				Name:        "X-Strangle-Token",
				Description: "Static auth.token, accepted only by POST /api/sync.",
			},
		}},
	}

	for _, pattern := range catalog.patterns {
		method, path, ok := splitPattern(pattern)
		if !ok || !documentedPath(path) {
			continue
		}
		key := method + " " + path
		meta, known := operationMetadata[key]
		if !known {
			// New routes appear in the document even before their metadata is
			// refined, so the spec cannot silently fall behind the router.
			meta = operationMeta{
				summary:   key,
				tags:      []string{"api"},
				auth:      docAuthUser,
				responses: map[int]responseMeta{200: result(200, "Successful response", map[string]any{})},
			}
		}
		op := buildOperation(method, path, meta)
		if spec.Paths[path] == nil {
			spec.Paths[path] = map[string]openAPIOp{}
		}
		spec.Paths[path][method] = op
	}
	return spec
}

func splitPattern(pattern string) (string, string, bool) {
	i := strings.IndexByte(pattern, ' ')
	if i <= 0 {
		return "", "", false
	}
	method := pattern[:i]
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return "", "", false
	}
	return method, strings.TrimSpace(pattern[i+1:]), true
}

func documentedPath(path string) bool {
	return strings.HasPrefix(path, apiPrefix+"/") || path == "/api/docs"
}

func buildOperation(method, path string, meta operationMeta) openAPIOp {
	op := openAPIOp{
		OperationID: operationID(method, path),
		Summary:     meta.summary,
		Tags:        meta.tags,
		Security:    securityFor(meta.auth),
		Responses:   map[string]openAPIResponse{},
	}
	for _, match := range pathParamPattern.FindAllStringSubmatch(path, -1) {
		op.Parameters = append(op.Parameters, openAPIParam{
			Name:        match[1],
			In:          "path",
			Description: pathParamDescription(match[1]),
			Required:    true,
			Schema:      pathParamSchema(match[1]),
		})
	}
	for _, q := range meta.queries {
		schema := schemaFor(q.kind)
		schema.Default = q.def
		op.Parameters = append(op.Parameters, openAPIParam{
			Name:        q.name,
			In:          "query",
			Description: q.description,
			Schema:      schema,
		})
	}
	if meta.request != nil {
		op.RequestBody = &openAPIRequestBody{
			Required: true,
			Content: map[string]openAPIMediaType{
				jsonMedia: {Schema: schemaFor(reflect.TypeOf(meta.request))},
			},
		}
	}
	for code, response := range meta.responses {
		mediaType := response.contentType
		if mediaType == "" {
			mediaType = jsonMedia
		}
		out := openAPIResponse{Description: response.description, Headers: map[string]openAPIHeader{}}
		for name, description := range response.headers {
			out.Headers[name] = openAPIHeader{Description: description, Schema: openAPISchema{Type: "integer"}}
		}
		if len(out.Headers) == 0 {
			out.Headers = nil
		}
		if response.body != nil {
			out.Content = map[string]openAPIMediaType{mediaType: {Schema: schemaFor(reflect.TypeOf(response.body))}}
		}
		op.Responses[fmt.Sprint(code)] = out
	}
	return op
}

func pathParamDescription(name string) string {
	switch name {
	case "id":
		return "Numeric run identifier"
	case "host":
		return "Inventory host name"
	case "org":
		return "Organization name"
	case "collection":
		return "Collection (target) name"
	case "name":
		return "Token name"
	default:
		return "Path parameter " + name
	}
}

func pathParamSchema(name string) openAPISchema {
	if name == "id" {
		return openAPISchema{Type: "integer", Format: "int64"}
	}
	return openAPISchema{Type: "string"}
}

func securityFor(authMode string) []map[string][]string {
	switch authMode {
	case docAuthOpen:
		return []map[string][]string{}
	case docAuthSync:
		return []map[string][]string{
			{"sessionCookie": {}},
			{"userBearer": {}},
			{"syncToken": {}},
		}
	default:
		return []map[string][]string{{"sessionCookie": {}}, {"userBearer": {}}}
	}
}

func operationID(method, path string) string {
	parts := []string{strings.ToLower(method)}
	for _, part := range strings.Split(path, "/") {
		part = strings.Trim(part, "{}")
		part = strings.NewReplacer("-", "_", ".", "_").Replace(part)
		switch part {
		case "", "api", apiVersion:
			continue
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "_")
}

var timeType = reflect.TypeOf(time.Time{})
var rawJSONType = reflect.TypeOf(json.RawMessage{})

func schemaFor(t reflect.Type) openAPISchema {
	return schemaForDepth(t, 0)
}

// schemaForDepth turns Go types into inline JSON schemas using their json
// tags. A depth cap protects against accidentally recursive types.
func schemaForDepth(t reflect.Type, depth int) openAPISchema {
	if depth > 12 {
		return openAPISchema{Type: "object"}
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType {
		return openAPISchema{Type: "string", Format: "date-time"}
	}
	if t == rawJSONType {
		return openAPISchema{Type: "object"}
	}

	switch t.Kind() {
	case reflect.String:
		return openAPISchema{Type: "string"}
	case reflect.Bool:
		return openAPISchema{Type: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		return openAPISchema{Type: "integer", Format: "int32"}
	case reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return openAPISchema{Type: "integer", Format: "int64"}
	case reflect.Float32, reflect.Float64:
		return openAPISchema{Type: "number"}
	case reflect.Slice, reflect.Array:
		item := schemaForDepth(t.Elem(), depth+1)
		return openAPISchema{Type: "array", Items: &item}
	case reflect.Map:
		value := schemaForDepth(t.Elem(), depth+1)
		return openAPISchema{Type: "object", AdditionalProperties: &value}
	case reflect.Struct:
		out := openAPISchema{Type: "object", Properties: map[string]openAPISchema{}}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if field.PkgPath != "" {
				continue
			}
			name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			out.Properties[name] = schemaForDepth(field.Type, depth+1)
			if !strings.Contains(options, "omitempty") && field.Type.Kind() != reflect.Pointer {
				out.Required = append(out.Required, name)
			}
		}
		return out
	default:
		return openAPISchema{Type: "object"}
	}
}
