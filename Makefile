BINARY := spot
PKG    := ./cmd/spot

GOFLAGS  ?=
LDFLAGS  ?= -s -w

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
