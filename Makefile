.PHONY: build run test test-integration fmt fmt-check lint vuln secure fuzz

# This module's own package dirs (go list stops at module boundaries, skips deps).
GO_DIRS := $(shell go list -f '{{.Dir}}' ./...)
GO_FILES := $(shell go list -f '{{range .GoFiles}}{{$$.Dir}}/{{.}} {{end}}{{range .TestGoFiles}}{{$$.Dir}}/{{.}} {{end}}{{range .XTestGoFiles}}{{$$.Dir}}/{{.}} {{end}}' ./...)

# Build the CodeRig binary.
build:
	mkdir -p $(HOME)/.looprig/bin
	CGO_ENABLED=0 go build -trimpath -o $(HOME)/.looprig/bin/coderig ./cmd/coderig

# Run the TUI directly. Optional .env values are launcher settings (for example,
# ACP executable path overrides), never provider credentials; models and inline
# keys belong only in the owner-only ~/.looprig/models.json.
run:
	set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/coderig

test:
	go test -race ./...

# Integration-tagged suite (process/filesystem/network/durable-storage-boundary
# tests, plus the live permission-review end-to-end tests). Not run by `test`
# or CI; run before any release touching those boundaries.
test-integration:
	go test -tags integration -race ./...

# Format this module's Go files in place.
fmt:
	gofmt -w $(GO_FILES)

# Fail if any Go file is not gofmt-clean.
fmt-check:
	@unformatted=$$(gofmt -l $(GO_FILES)); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed (run 'make fmt'):"; echo "$$unformatted"; exit 1; \
	fi

lint: fmt-check
	go vet ./...
	go tool staticcheck ./...
	go tool gosec $(GO_DIRS)

vuln:
	go mod verify
	go tool govulncheck ./...

secure: lint vuln

fuzz:
	@echo "Usage: go test -fuzz=FuzzXxx ./path/to/pkg -fuzztime=30s"
