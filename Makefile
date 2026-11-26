GO := go

# go.nhat.io/cpy/v3 asks pkg-config for python-3.12-embed; map it to the
# python3-embed package installed on this machine so Go builds work with any
# CPython version that provides the development headers.
export PKG_CONFIG := $(CURDIR)/scripts/pkg-config-cpy

# swaggo/swag v2 is pinned as a Go tool in go.mod; invoke it via `go tool`.
SWAG := $(GO) tool swag
SWAG_ARGS := init --v3.1 -g api/openapi_general.go -d . -o ./api/docs --parseDependency

.PHONY: all build run test coverage tidy vet ui ui-dev swag swag-check generate

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

coverage: swag
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

vet: swag
	$(GO) vet ./...

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
