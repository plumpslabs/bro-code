# bro-code build tooling — keep it minimal.
#
# Build injects version/commit/date via ldflags (see cmd/brocode/main.go).

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

BINARY := bin/brocode

.PHONY: all build build-all test vet fmt check run measure install clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/brocode

build-all:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/brocode-linux-amd64 ./cmd/brocode
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/brocode-linux-arm64 ./cmd/brocode
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/brocode-windows-amd64.exe ./cmd/brocode
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/brocode-darwin-amd64 ./cmd/brocode
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/brocode-darwin-arm64 ./cmd/brocode

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

# Install to the Go bin dir (~/go/bin) — user-writable and already on PATH
# for Go developers. Override with: make install BINDIR=/usr/local/bin
BINDIR ?= $(shell go env GOPATH)/bin

install: build
	install -d $(BINDIR)
	install -m 0755 $(BINARY) $(BINDIR)/brocode

clean:
	rm -rf bin
