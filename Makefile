BINARY := spot
PKG    := ./cmd/spot

# Version metadata baked into the binary via -ldflags. Override on the
# command line if the git context isn't available (e.g. tarball build).
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

GOFLAGS  ?=
LDFLAGS  ?= -s -w \
            -X main.version=$(VERSION) \
            -X main.commit=$(COMMIT) \
            -X main.buildDate=$(BUILD_DATE)

.PHONY: all build install test vet fmt tidy clean

all: build

build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) $(PKG)

install:
	go install $(GOFLAGS) -ldflags '$(LDFLAGS)' $(PKG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

clean:
	rm -f $(BINARY)
