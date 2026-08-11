.PHONY: test fmt fmt-check vendor vendor-check lint vuln verify secure fuzz

# Module's own package dirs, excluding vendor/ and the nested .worktrees/ modules
# (go list ./... stops at nested module boundaries and skips vendor). GO_DIRS is
# intentionally reserved for tools such as gosec that accept package dirs:
# Go 1.26 gofmt recursively walks directory operands and would mutate vendor/.
GO_DIRS := $(shell go list -f '{{.Dir}}' ./...)

# Build from the vendored dependency tree: offline, reproducible, and auditable
# (every dependency's source lives in vendor/ and shows up in review diffs). Go
# auto-selects -mod=vendor when vendor/ is present; we export it explicitly so a
# stray global GOFLAGS (e.g. -mod=mod) can't silently switch the build off the
# vendored tree. Do NOT use -mod=readonly here — it ignores vendor/ entirely.
export GOFLAGS := -mod=vendor

test:
	go test -race ./...

# Refresh the checked-in dependency tree and reject repository metadata copied
# from local replace targets. A nested .git file can contain an absolute worktree
# path while remaining invisible to the parent repository's git status.
vendor:
	go mod vendor
	# Go copies the regular .git pointer from the local inference replace target.
	# Remove only that known artifact; vendor-check rejects directories or any
	# additional repository metadata instead of silently deleting them.
	$(RM) vendor/github.com/looprig/inference/.git
	@$(MAKE) --no-print-directory vendor-check

vendor-check:
	@set -e; \
	hidden_git=$$(find vendor -name .git -print -quit); \
	if [ -n "$$hidden_git" ]; then \
		echo "forbidden Git metadata in vendor tree:" >&2; \
		printf '%s\n' "$$hidden_git" >&2; \
		exit 1; \
	fi

# Format every Go file directly in each first-party package directory. The find
# depth includes platform-specific files while excluding vendor and nested
# module trees. Quoted, line-delimited package dirs preserve spaces in paths.
fmt:
	@set -e; \
	dirs=$$(go list -f '{{.Dir}}' ./...); \
	if [ -z "$$dirs" ]; then exit 0; fi; \
	printf '%s\n' "$$dirs" | while IFS= read -r dir; do \
		find "$$dir" -maxdepth 1 -type f -name '*.go' -exec gofmt -w {} + || exit $$?; \
	done

# Fail (non-zero exit) if any tracked Go file is not gofmt-clean. Wired into lint.
fmt-check:
	@set -e; \
	dirs=$$(go list -f '{{.Dir}}' ./...); \
	if [ -z "$$dirs" ]; then exit 0; fi; \
	unformatted=$$(printf '%s\n' "$$dirs" | while IFS= read -r dir; do \
		find "$$dir" -maxdepth 1 -type f -name '*.go' -exec gofmt -l {} + || exit $$?; \
	done); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed (run 'make fmt'):"; echo "$$unformatted"; exit 1; \
	fi

lint: fmt-check vendor-check
	go vet ./...
	go run -mod=mod honnef.co/go/tools/cmd/staticcheck ./...
	# gosec is NOT module-aware: its ./... is a filesystem walk that descends into
	# the nested .worktrees/ checkouts (separate modules) and, under -mod=vendor,
	# reports modules.txt desyncs for those foreign trees. Scope it to THIS module's
	# package dirs via GO_DIRS (the same go-list idiom fmt/fmt-check use). go vet and
	# staticcheck are module-aware (go list stops at module boundaries), so they need
	# no scoping.
	go run -mod=mod github.com/securego/gosec/v2/cmd/gosec $(GO_DIRS)

vuln:
	go mod verify
	go run -mod=mod golang.org/x/vuln/cmd/govulncheck ./...

secure: lint vuln

fuzz:
	@echo "Usage: go test -fuzz=FuzzXxx ./path/to/pkg -fuzztime=30s"
