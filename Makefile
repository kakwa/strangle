GO := go

# go.nhat.io/cpy/v3 asks pkg-config for python-3.12-embed; map it to the
# python3-embed package installed on this machine so Go builds work with any
# CPython version that provides the development headers.
export PKG_CONFIG := $(CURDIR)/scripts/pkg-config-cpy

# swaggo/swag v2, golangci-lint and govulncheck are pinned as Go tools in
# go.mod; invoke them via `go tool`.
SWAG := $(GO) tool swag
SWAG_ARGS := init --v3.1 -g api/openapi_general.go -d . -o ./api/docs --parseDependency
LINT := $(GO) tool golangci-lint
VULN := $(GO) tool govulncheck

.PHONY: all build run test coverage tidy vet lint vulncheck ui ui-dev swag swag-check generate

all: build

# Build the UI into ui/dist first: the output is embedded into the server
# binary at compile time (see ui/ui.go), so a stale or missing dist would
# end up baked into bin/strangle-server.
build: swag ui
	$(GO) build -o bin/strangle-server ./cmd/strangle-server
	$(GO) build -o bin/strangle-cli ./cmd/strangle-cli

run: build
	./bin/strangle-server serve

test: swag
	$(GO) test ./...

# -coverpkg=./... is essential: each test binary otherwise only instruments
# its own package, so the integration suites (which have no statements of
# their own) would contribute nothing to the coverage of api, model, ...
coverage: swag
	$(GO) test -coverpkg=./... -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1
	$(GO) tool cover -html=coverage.out -o coverage.html

vet: swag
	$(GO) vet ./...

# Lint the whole module with golangci-lint (pinned in go.mod).
lint: swag
	$(LINT) run ./...

# Scan direct and indirect dependencies for known vulnerabilities
# (golang.org/x/vuln, pinned in go.mod). Needs network access to the
# vulnerability database.
vulncheck:
	$(VULN) ./...

tidy:
	$(GO) mod tidy

# Regenerate the checked-in OpenAPI 3.1 document from swag annotations.
swag:
	$(SWAG) $(SWAG_ARGS)

generate: swag

swag-check:
	$(MAKE) swag
	git diff --exit-code -- api/docs/docs.go api/docs/swagger.json api/docs/swagger.yaml

# Build the UI into ui/dist (served by the strangle server).
ui:
	cd ui && npm install && npm run build

ui-dev:
	cd ui && npm run dev
