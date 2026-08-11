.PHONY: test fmt fmt-check vet staticcheck gosec vuln secure check

# Every Go command runs with GOWORK=off so the parent go.work never captures this
# module (see CLAUDE.md).

test:
	GOWORK=off go test -race ./...

# Format the whole module in place.
fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

vet:
	GOWORK=off go vet ./...

# staticcheck, gosec, and govulncheck are NOT module dependencies — CLAUDE.md
# forbids adding anything beyond storage to go.mod — so each is invoked as an
# external binary resolved from PATH or GOPATH/bin. If neither has it, the
# target warns and skips so `make secure` stays green where the tool is not
# installed.
staticcheck:
	@STATICCHECK=$$(command -v staticcheck || echo "$$(go env GOPATH)/bin/staticcheck"); \
	if [ -x "$$STATICCHECK" ]; then \
		echo "staticcheck: $$STATICCHECK"; GOWORK=off "$$STATICCHECK" ./...; \
	else \
		echo "staticcheck not installed; skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"; \
	fi

# gosec scans for security issues. It is NOT a module dependency — CLAUDE.md forbids
# adding anything beyond storage to go.mod — so it is invoked as an external
# binary resolved from PATH or GOPATH/bin. If neither has it, the target warns and
# skips so `make secure` stays green where gosec is not installed; install with:
#   go install github.com/securego/gosec/v2/cmd/gosec@latest
gosec:
	@GOSEC=$$(command -v gosec || echo "$$(go env GOPATH)/bin/gosec"); \
	if [ -x "$$GOSEC" ]; then \
		echo "gosec: $$GOSEC"; GOWORK=off "$$GOSEC" -quiet ./...; \
	else \
		echo "gosec not installed; skipping (go install github.com/securego/gosec/v2/cmd/gosec@latest)"; \
	fi

vuln:
	@GOVULNCHECK=$$(command -v govulncheck || echo "$$(go env GOPATH)/bin/govulncheck"); \
	if [ -x "$$GOVULNCHECK" ]; then \
		echo "govulncheck: $$GOVULNCHECK"; GOWORK=off "$$GOVULNCHECK" ./...; \
	else \
		echo "govulncheck not installed; skipping (go install golang.org/x/vuln/cmd/govulncheck@latest)"; \
	fi

# secure is a superset of check: everything check runs, plus the PATH-resolved
# lint/security tools above.
secure: fmt-check vet staticcheck gosec vuln

check: fmt-check vet test
