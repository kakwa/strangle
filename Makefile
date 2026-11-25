GO := go

# go.nhat.io/cpy/v3 asks pkg-config for python-3.12-embed; map it to the
# python3-embed package installed on this machine so Go builds work with any
# CPython version that provides the development headers.
export PKG_CONFIG := $(CURDIR)/scripts/pkg-config-cpy

SWAG := $(GO) run github.com/swaggo/swag/v2/cmd/swag
SWAG_ARGS := init --v3.1 -g internal/server/openapi_general.go -d . -o ./docs --parseDependency --parseInternal

.PHONY: all build run test coverage tidy vet web web-dev swag swag-check generate

all: build

# Build the web UI into web/dist first: the output is embedded into the server
# binary at compile time (see web/web.go), so a stale or missing dist would
# end up baked into bin/strangle-server.
build: web
	$(GO) build -o bin/strangle-server ./cmd/strangle-server
	$(GO) build -o bin/strangle-cli ./cmd/strangle-cli

run: build
	./bin/strangle-server serve

test:
	$(GO) test ./...

coverage:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

# Regenerate the checked-in OpenAPI 3.1 document from swag annotations.
swag:
	$(SWAG) $(SWAG_ARGS)

generate: swag

swag-check:
	$(MAKE) swag
	git diff --exit-code -- docs/docs.go docs/swagger.json docs/swagger.yaml

# Build the web UI into web/dist (served by the strangle server).
web:
	cd web && npm install && npm run build

web-dev:
	cd web && npm run dev
