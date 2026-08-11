# bro-code build tooling — keep it minimal.
#
# Build injects version/commit/date via ldflags (see cmd/brocode/main.go).

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

BINARY := bin/brocode

.PHONY: all build test vet fmt check run measure install clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/brocode

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w ./cmd ./internal

check: vet
	@test -z "$$(gofmt -l ./cmd ./internal)" || (echo "gofmt needed:"; gofmt -l ./cmd ./internal; exit 1)

run:
	go run ./cmd/brocode

# Measure footprint (ritual from docs/TECH_STACK.md "Measured footprint").
measure: build
	@ls -lh $(BINARY) | awk '{print "binary size:", $$5}'
	@/usr/bin/time -p ./$(BINARY) --search mcp >/dev/null

install: build
	install -m 0755 $(BINARY) /usr/local/bin/brocode

clean:
	rm -rf bin
