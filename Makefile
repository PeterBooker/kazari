# Agalma build targets. CGO is never needed: the SQLite driver is pure Go.

BINARY  ?= agalma
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  = -s -w \
  -X agalma/internal/buildinfo.version=$(VERSION) \
  -X agalma/internal/buildinfo.commit=$(COMMIT) \
  -X agalma/internal/buildinfo.date=$(DATE)

export CGO_ENABLED = 0

.PHONY: build test lint vet vuln run-dev release snapshot tidy clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/agalma

test:
	go test -race ./...

vet:
	go vet ./...

lint: vet
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Serves http://demo.localhost:8080 style URLs in proxy mode with a local data dir.
run-dev: build
	AGALMA_BASE_DOMAIN=localhost \
	AGALMA_DATA_DIR=./.data \
	AGALMA_LISTEN_PROXY_MODE=true \
	AGALMA_LISTEN_HTTP=:8080 \
	AGALMA_EXTERNAL_SCHEME=http \
	AGALMA_EXTERNAL_PORT=8080 \
	AGALMA_TLS_MODE=self-signed \
	AGALMA_AUTH_ADMIN_TOKEN=dev-admin-token \
	./$(BINARY) serve

release:
	goreleaser release --clean

snapshot:
	goreleaser release --snapshot --clean --skip=publish,sign,sbom

tidy:
	go mod tidy

clean:
	rm -rf $(BINARY) dist .data
