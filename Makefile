.PHONY: build install run test test-integration fmt fmt-check lint vuln secure fuzz

# This module's own package dirs (go list stops at module boundaries, skips deps).
GO_DIRS := $(shell go list -f '{{.Dir}}' ./...)
GO_FILES := $(shell go list -f '{{range .GoFiles}}{{$$.Dir}}/{{.}} {{end}}{{range .TestGoFiles}}{{$$.Dir}}/{{.}} {{end}}{{range .XTestGoFiles}}{{$$.Dir}}/{{.}} {{end}}' ./...)

# Build the Carbon binary into this repo's own (gitignored) bin/ dir. This is a
# local dev-iteration artifact, deliberately NOT on PATH -- use `make install` to
# put a runnable carbon on PATH.
build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/carbon ./cmd/carbon

# Build and install the Carbon binary onto PATH at ~/.looprig/bin/carbon. Copies to a
# temp file then renames into place rather than overwriting the destination's bytes
# in-place: macOS's kernel caches code-signature validation per inode, and overwriting a
# previously-executed binary in place leaves that cache stale, so the NEW content gets
# killed at launch with "load code signature error 2" until the file is deleted and
# recreated. rename(2) on the same filesystem gives the destination path a fresh inode,
# which avoids the stale cache entirely.
install: build
	mkdir -p $(HOME)/.looprig/bin
	cp bin/carbon $(HOME)/.looprig/bin/carbon.new
	mv -f $(HOME)/.looprig/bin/carbon.new $(HOME)/.looprig/bin/carbon

# Run the TUI directly. Optional .env values are launcher settings (for example,
# ACP executable path overrides), never provider credentials; models and inline
# keys belong only in the owner-only ~/.looprig/carbon/models.json.
run:
	set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/carbon

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

# --- standardized check surface -------------------------------------------
# One target, the same set of checks, in every module. CI calls exactly this,
# so a check can no longer pass locally and be silently absent in CI (or the
# reverse). The lint/security tools are pinned by this module's go.mod tool directives.
#
# CHECK_GO_DIRS scopes gosec: gosec is NOT module-aware, so a bare ./... is a
# filesystem walk that descends into nested .worktrees/ checkouts, which are
# separate modules. go vet and staticcheck are module-aware and need no scope.
CHECK_GO_DIRS = $(shell GOWORK=off go list -f '{{.Dir}}' ./...)
# CHECK_GO_FILES is what gofmt gets. Never hand it CHECK_GO_DIRS: gofmt RECURSES
# into directory operands, so for a module with a root package it would walk the
# whole tree, nested .worktrees/ checkouts included.
CHECK_GO_FILES = $(foreach dir,$(CHECK_GO_DIRS),$(wildcard $(dir)/*.go))

vet:
	GOWORK=off go vet ./...

check-staticcheck:
	GOWORK=off go tool staticcheck ./...

check-gosec:
	GOWORK=off go tool gosec -quiet $(CHECK_GO_DIRS)

check-vuln:
	GOWORK=off go mod verify
	GOWORK=off go tool govulncheck ./...

check: fmt-check vet check-staticcheck check-gosec check-vuln test build

.PHONY: check check-staticcheck check-gosec check-vuln fmt fmt-check vet test build
