BINARY   := krill
DIST     := ./dist
GO       := go
LDFLAGS  := -trimpath -ldflags="-s -w"

.PHONY: all build test lint run clean tidy docker help

all: tidy test build

## tidy: download dependencies
tidy:
	$(GO) mod tidy

## build: compile static binary → ./dist/krill
build:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o $(DIST)/$(BINARY) ./cmd/krill/
	@echo "✓ $(DIST)/$(BINARY)"

## test: run all tests with race detector
test:
	$(GO) test -race -count=1 -timeout=60s ./...

## lint: vet + staticcheck
lint:
	$(GO) vet ./...
	@which staticcheck > /dev/null 2>&1 && staticcheck ./... || echo "(install: go install honnef.co/go/tools/cmd/staticcheck@latest)"

## run: build and start with default config
run: build
	$(DIST)/$(BINARY) -config krill.yaml

## clean: remove build artifacts
clean:
	rm -rf $(DIST)

## docker: build minimal distroless image
docker:
	docker build -t krill:latest .

## help: print this help
help:
	@grep -E '^## ' Makefile | sed 's/## /  /'
