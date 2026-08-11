.PHONY: test test-integration check fmt fmt-check vendor vendor-scrub vendor-check lint vuln secure fuzz

# Module's own package dirs, excluding vendor/ and the nested .worktrees/ modules
# (go list ./... stops at nested module boundaries and skips vendor).
GO_DIRS := $(shell go list -f '{{.Dir}}' ./...)
GO_FILES := $(shell go list -f '{{range .GoFiles}}{{$$.Dir}}/{{.}} {{end}}{{range .TestGoFiles}}{{$$.Dir}}/{{.}} {{end}}{{range .XTestGoFiles}}{{$$.Dir}}/{{.}} {{end}}' ./...)

# Build from the vendored dependency tree: offline, reproducible, and auditable
# (every dependency's source lives in vendor/ and shows up in review diffs). Go
# auto-selects -mod=vendor when vendor/ is present; we export it explicitly so a
# stray global GOFLAGS (e.g. -mod=mod) can't silently switch the build off the
# vendored tree. Do NOT use -mod=readonly here — it ignores vendor/ entirely.
# NOTE: -mod=vendor is incompatible with an active go.work workspace. This module
# participates in ../go.work for local editing, so run targets with the workspace
# disabled: `GOWORK=off make secure`.
export GOFLAGS := -mod=vendor

# Go 1.26 does not resolve declared `go tool` binaries from a vendor tree. Keep
# application builds on the auditable vendor tree, but resolve development-only
# analysis binaries through the pinned module graph.
TOOL_GOFLAGS := -mod=mod

VENDOR_DIR ?= vendor
LOCAL_REPLACE_VENDOR_DIRS := \
	$(VENDOR_DIR)/github.com/looprig/core \
	$(VENDOR_DIR)/github.com/looprig/harness \
	$(VENDOR_DIR)/github.com/looprig/inference \
	$(VENDOR_DIR)/github.com/looprig/storage

test:
	go test -race ./...

check: test vendor-check

# Run the integration-tagged tests (process-boundary seams excluded from the
# default run): scoped to this module's package dirs, same GO_DIRS idiom as above.
test-integration:
	go test -tags integration -race $(GO_DIRS)

# Format the whole module in place.
fmt:
	gofmt -w $(GO_FILES)

# Fail (non-zero exit) if any tracked Go file is not gofmt-clean. Wired into lint.
fmt-check:
	@unformatted=$$(gofmt -l $(GO_FILES)); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed (run 'make fmt'):"; echo "$$unformatted"; exit 1; \
	fi

# Refresh the auditable dependency tree, remove VCS metadata donated by local
# replacements, then reject any metadata left by an unexpected source.
vendor:
	go mod vendor
	$(MAKE) vendor-scrub
	$(MAKE) vendor-check

vendor-scrub:
	rm -rf $(addsuffix /.git,$(LOCAL_REPLACE_VENDOR_DIRS))

vendor-check:
	@metadata=$$(find "$(VENDOR_DIR)" -name .git -print); \
	if [ -n "$$metadata" ]; then \
		echo "forbidden VCS metadata in $(VENDOR_DIR):"; echo "$$metadata"; exit 1; \
	fi

lint: fmt-check vendor-check
	go vet ./...
	GOFLAGS=$(TOOL_GOFLAGS) go tool staticcheck ./...
	# gosec is NOT module-aware: its ./... is a filesystem walk that descends into
	# the nested .worktrees/ checkouts (separate modules) and, under -mod=vendor,
	# reports modules.txt desyncs for those foreign trees. Scope it to THIS module's
	# package dirs via GO_DIRS (the same go-list idiom fmt/fmt-check use). go vet and
	# staticcheck are module-aware (go list stops at module boundaries), so they need
	# no scoping.
	GOFLAGS=$(TOOL_GOFLAGS) go tool gosec $(GO_DIRS)

vuln:
	go mod verify
	GOFLAGS=$(TOOL_GOFLAGS) go tool govulncheck ./...

secure: lint vuln

fuzz:
	@echo "Usage: go test -fuzz=FuzzXxx ./path/to/pkg -fuzztime=30s"
