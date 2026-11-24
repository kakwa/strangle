GO := go

.PHONY: all build run test coverage tidy vet web web-dev

all: build

build:
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
