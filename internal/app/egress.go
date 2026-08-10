package app

import (
	"fmt"
	"strings"

	"github.com/looprig/sandbox"
)

// egress.go resolves the parent process's HTTP/HTTPS proxy environment into one
// validated, immutable session egress route. Upstream credentials are captured
// only inside the returned sandbox.EgressRoute (its private URL) and never enter
// the fingerprint, logs, prompts, permission file, audit records, or child
// environment. NO_PROXY is validated and only ever honored as an explicit direct
// route; it never silently downgrades a configured upstream into a direct
// connection.

// EgressResolution is the parent-resolved, secret-free session egress policy.
// Route is the single validated route assembly passes to sandbox
// WithEgressRoute; its Fingerprint and guarantee bits are the non-secret
// fingerprint inputs. Upstream is true when an organization proxy was captured
// (used only for diagnostics, never carrying credentials).
type EgressResolution struct {
	Route    sandbox.EgressRoute
	Upstream bool
}

// EgressRouteError reports a fail-closed egress-resolution failure. It carries a
// bounded, non-secret reason: a raw proxy URL (which may embed credentials) is
// never placed in the message.
type EgressRouteError struct {
	Reason string
	Cause  error
}

func (e *EgressRouteError) Error() string {
	if e.Cause != nil {
		return "carbon: egress route: " + e.Reason + ": " + e.Cause.Error()
	}
	return "carbon: egress route: " + e.Reason
}

func (e *EgressRouteError) Unwrap() error { return e.Cause }

// resolveEgressRoute captures the parent's proxy configuration through the
// injected environment getter and produces one validated session route:
//
//   - No HTTP(S) proxy configured -> an explicit direct route (local DNS and
//     address-class validation). NO_PROXY is moot; a direct route already
//     reaches every target through sandbox's local target-enforcement proxy.
//   - HTTP(S) proxy configured, NO_PROXY empty -> an upstream (organization
//     proxy) route. Credentials in the proxy URL are retained ONLY inside the
//     route object.
//   - HTTP(S) proxy configured, NO_PROXY == "*" -> an explicit, validated direct
//     route (the operator has explicitly disabled proxying for all targets).
//   - HTTP(S) proxy configured with specific NO_PROXY entries -> fail closed.
//     The v1 single-route model cannot honor per-target direct exceptions, and
//     silently bypassing the upstream for those targets (or silently ignoring
//     the exceptions) is exactly the direct fallback the spec forbids. The
//     operator must choose a direct route or a full upstream route.
//
// Malformed proxy URLs and malformed NO_PROXY entries fail closed.
func resolveEgressRoute(getenv func(string) string) (EgressResolution, error) {
	if getenv == nil {
		return EgressResolution{}, &EgressRouteError{Reason: "no environment getter"}
	}

	upstream := firstNonEmpty(getenv, "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy")
	noProxy := firstNonEmpty(getenv, "NO_PROXY", "no_proxy")

	entries, wildcard, err := parseNoProxy(noProxy)
	if err != nil {
		return EgressResolution{}, err
	}

	// No upstream proxy: a direct route reaches every target.
	if upstream == "" {
		route, derr := sandbox.NewDirectEgressRoute()
		if derr != nil {
			return EgressResolution{}, &EgressRouteError{Reason: "direct route rejected", Cause: derr}
		}
		return EgressResolution{Route: route}, nil
	}

	// Upstream configured but explicitly disabled for all targets: honor the
	// explicit direct policy.
	if wildcard {
		route, derr := sandbox.NewDirectEgressRoute()
		if derr != nil {
			return EgressResolution{}, &EgressRouteError{Reason: "direct route rejected", Cause: derr}
		}
		return EgressResolution{Route: route}, nil
	}

	// Upstream configured with specific NO_PROXY exceptions: not expressible
	// through the v1 single route. Fail closed rather than bypass silently.
	if len(entries) > 0 {
		return EgressResolution{}, &EgressRouteError{
			Reason: fmt.Sprintf("per-target NO_PROXY exceptions (%d) are not supported with an upstream proxy in v1; select a direct or full-upstream route", len(entries)),
		}
	}

	// trustedAddressGuarantee is false: an organization proxy resolves DNS and we
	// cannot independently guarantee the resolved address class. Hostname/port
	// enforcement is still guaranteed by the local proxy.
	route, rerr := sandbox.NewUpstreamEgressRoute(upstream, false)
	if rerr != nil {
		// rerr may quote the raw URL (with credentials); do not wrap it.
		return EgressResolution{}, &EgressRouteError{Reason: "upstream proxy configuration is invalid"}
	}
	return EgressResolution{Route: route, Upstream: true}, nil
}

// firstNonEmpty returns the first trimmed, non-empty value among names.
func firstNonEmpty(getenv func(string) string, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

// parseNoProxy validates a comma-separated NO_PROXY value. It returns the
// validated non-wildcard entries, whether an unconditional "*" wildcard is
// present, and an error for any malformed entry. Whitespace-only segments are
// ignored; an entry containing a scheme, credentials, path, or internal
// whitespace is rejected so a malformed exception cannot be interpreted loosely.
func parseNoProxy(raw string) (entries []string, wildcard bool, err error) {
	if strings.TrimSpace(raw) == "" {
		return nil, false, nil
	}
	for _, segment := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(segment)
		if entry == "" {
			continue
		}
		if entry == "*" {
			wildcard = true
			continue
		}
		if strings.ContainsAny(entry, " \t/@") || strings.Contains(entry, "://") {
			// Do not echo the entry: a fat-fingered credential-bearing proxy URL
			// (user:pw@host) matches exactly this branch and must not reach stderr.
			return nil, false, &EgressRouteError{Reason: "malformed NO_PROXY entry: contains a scheme, credentials, path, or whitespace"}
		}
		entries = append(entries, entry)
	}
	return entries, wildcard, nil
}
