.PHONY: build install run test test-integration fmt fmt-check lint vuln secure fuzz

# This module's own package dirs (go list stops at module boundaries, skips deps).
GO_DIRS := $(shell go list -f '{{.Dir}}' ./...)
GO_FILES := $(shell go list -f '{{range .GoFiles}}{{$$.Dir}}/{{.}} {{end}}{{range .TestGoFiles}}{{$$.Dir}}/{{.}} {{end}}{{range .XTestGoFiles}}{{$$.Dir}}/{{.}} {{end}}' ./...)

# Build the CodeRig binary into this repo's own (gitignored) bin/ dir. This is a
# local dev-iteration artifact, deliberately NOT on PATH -- use `make install` to
# put a runnable coderig on PATH.
build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/coderig ./cmd/coderig

# Build and install the CodeRig binary onto PATH at ~/.looprig/bin/coderig. Copies to a
# temp file then renames into place rather than overwriting the destination's bytes
# in-place: macOS's kernel caches code-signature validation per inode, and overwriting a
# previously-executed binary in place leaves that cache stale, so the NEW content gets
# killed at launch with "load code signature error 2" until the file is deleted and
# recreated. rename(2) on the same filesystem gives the destination path a fresh inode,
# which avoids the stale cache entirely.
install: build
	mkdir -p $(HOME)/.looprig/bin
	cp bin/coderig $(HOME)/.looprig/bin/coderig.new
	mv -f $(HOME)/.looprig/bin/coderig.new $(HOME)/.looprig/bin/coderig

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
