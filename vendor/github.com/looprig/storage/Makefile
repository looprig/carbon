.PHONY: test fmt fmt-check vet staticcheck gosec vuln secure check

test:
	GOWORK=off go test -race ./...

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

vet:
	GOWORK=off go vet ./...

# staticcheck, gosec, and govulncheck are NOT module dependencies — CLAUDE.md
# forbids adding anything beyond stdlib to go.mod — so each is invoked as an
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

secure: fmt-check vet staticcheck gosec vuln

check: fmt-check vet test
