VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS  = -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build install test

build:
	go build -ldflags '$(LDFLAGS)' -o demonolith .

# Installs into GOBIN (default ~/go/bin).
install:
	go install -ldflags '$(LDFLAGS)' .

test:
	go test ./...
