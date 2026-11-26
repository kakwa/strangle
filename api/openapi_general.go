package api

// General API annotations consumed by `swag init -g api/openapi_general.go`.
//
// Each security scheme lives in its own comment group: swaggo/swag v2 derives
// every scheme key in a group from the first @securityDefinitions annotation
// in that group, so defining several schemes in one comment block would make
// them overwrite each other. The unexported functions only anchor the
// comments; they are never called.

// @title            Strangle API
// @version          v1
// @description      Versioned HTTP API for inspecting and administering Ansible playbook collections.
// @license.name     GPL-3.0
// @servers.url      /
func swaggerGeneralInfo() {} //nolint:unused // anchors the swag annotation group above

// @securityDefinitions.apikey sessionCookie
// @in                         cookie
// @name                       strangle_session
// @description                Session cookie returned by POST /api/v1/login.
func swaggerSessionCookieScheme() {} //nolint:unused // anchors the swag annotation group above

// @securityDefinitions.apikey syncToken
// @in                         header
// @name                       X-Strangle-Token
// @description                Static auth.token, accepted by collection hook endpoints.
func swaggerSyncTokenScheme() {} //nolint:unused // anchors the swag annotation group above

// @securityDefinitions.bearerauth userBearer
// @bearerFormat                  st_
// @description                   Collection-scoped personal access token created through POST /api/v1/orgs/{org}/collections/{collection}/tokens.
func swaggerUserBearerScheme() {} //nolint:unused // anchors the swag annotation group above
