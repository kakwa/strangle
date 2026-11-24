GO := go

# go.nhat.io/cpy/v3 asks pkg-config for python-3.12-embed; map it to the
# python3-embed package installed on this machine so Go builds work with any
# CPython version that provides the development headers.
export PKG_CONFIG := $(CURDIR)/scripts/pkg-config-cpy

.PHONY: all build run test coverage tidy vet web web-dev

all: build

# Build the web UI into web/dist first: the output is embedded into the Go
# binary at compile time (see web/web.go), so a stale or missing dist would
# end up baked into bin/strangle.
build: web
	$(GO) build -o bin/strangle ./cmd/strangle

run: build
	./bin/strangle

test:
	$(GO) test ./...

coverage:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

# Build the web UI into web/dist (served by the strangle server).
web:
	cd web && npm install && npm run build

web-dev:
	cd web && npm run dev
