// This file is the home Task 3.1 flagged for origin canonicalization.
//
// Key.Validate rejects a non-canonical ServerOrigin rather than normalizing it,
// on the grounds that a store keyed by a value the caller did not choose is a
// surprising store. That is the right call for a validator, but it leaves
// somebody holding a real URL — "https://Example.COM:0443/mcp?x=1", read from a
// config file — with no sanctioned way to turn it into a Key. This is that way.
//
// The division of labor is deliberate and the two halves must not be merged:
//
//   - CanonicalOrigin NORMALIZES what is unambiguously the same origin spelled
//     differently (case, default port, leading zeros, trailing root label,
//     path/query/fragment), and REJECTS what is ambiguous or dangerous
//     (userinfo, a non-ASCII host, an IPv6 zone, a non-loopback http URL).
//   - Key.Validate demands the result.
//
// So the caller canonicalizes once, deliberately, at the boundary where it
// still knows what the URL meant — and every layer below can assume canonical.
// The invariant tying them together is tested: anything CanonicalOrigin returns
// passes Key.Validate, and canonicalizing it again is a no-op.

package auth

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// MaxURLBytes bounds the input to CanonicalOrigin.
//
// It is larger than MaxOriginBytes because the input is a whole URL — the
// caller's real server URL, path and query included — while the output is only
// its origin. The output is bounded separately, against MaxOriginBytes, so a
// long path cannot produce an over-long Key.
const MaxURLBytes = 2048

// CanonicalOrigin reduces rawURL to the canonical origin a Key requires:
// scheme://host[:port], lowercase, with no default port, path, query, fragment,
// or userinfo. This is RFC 6454 origin serialization.
//
// Violations are returned as *Error with class ClassInvalidConfig. The result,
// when err is nil, always satisfies Key.Validate and is idempotent under a
// second call.
//
// What it normalizes, and why each is safe to do silently: these are all cases
// where two spellings are provably the same origin, so normalizing costs the
// caller nothing and NOT normalizing costs a duplicate store entry and a
// redundant interactive login.
//
//	HTTPS://Example.COM./mcp?q=1#f  ->  https://example.com
//	https://example.com:0443        ->  https://example.com
//	http://127.0.0.1:8080/mcp       ->  http://127.0.0.1:8080
//
// What it refuses, and why each is NOT safe to normalize:
//
//   - userinfo — "https://user:pw@h" carries a credential. Silently dropping it
//     would discard something the caller meant, and keeping it is not an origin.
//     Only the caller knows which it wanted, so it must say.
//   - a non-ASCII host — converting to punycode needs golang.org/x/net/idna,
//     which is not a sanctioned dependency; and a Unicode homograph reaching a
//     log line through Key.String is its own problem. The caller encodes the
//     A-label, because the caller is what knows the name.
//   - an IPv6 zone identifier — scoped to one machine's interfaces, so not an
//     identity a token can be keyed by.
//   - http to a non-loopback host — tokens do not cross a cleartext network.
//     This mirrors Key.Validate and what the HTTP transport will require.
func CanonicalOrigin(rawURL string) (string, error) {
	fail := func(msg string) (string, error) {
		return "", NewError(ClassInvalidConfig, "canonical_origin", msg, nil)
	}

	if rawURL == "" {
		return fail("URL is empty")
	}
	if len(rawURL) > MaxURLBytes {
		return fail(fmt.Sprintf("URL is %d bytes, max %d", len(rawURL), MaxURLBytes))
	}
	// Checked before parsing: url.Parse tolerates some control characters, and
	// the message below is destined for a log line.
	if i := indexOfControl(rawURL); i >= 0 {
		return fail(fmt.Sprintf("URL contains a control character at index %d", i))
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		// err quotes the URL, which may carry userinfo. Do not wrap it.
		return fail("URL is not a valid URL")
	}
	if u.Opaque != "" {
		return fail("URL must be scheme://host[:port], not an opaque URL")
	}
	if u.User != nil {
		return fail("URL must have no userinfo; supply credentials through the OAuth client, not the URL")
	}

	host, err := canonicalHost(u.Hostname())
	if err != nil {
		return fail(err.Error())
	}
	port, err := canonicalPort(u.Scheme, u.Port())
	if err != nil {
		return fail(err.Error())
	}

	// url.Parse lowercases the scheme already; comparing lowercase is defensive
	// rather than load-bearing, and costs nothing.
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(host) {
			return fail(fmt.Sprintf("URL scheme http is allowed only for loopback hosts, got host %q", host))
		}
	case "":
		return fail("URL has no scheme; want https://host[:port]")
	default:
		return fail(fmt.Sprintf("URL scheme must be https (or http for loopback), got %q", u.Scheme))
	}

	// Path, query and fragment are discarded rather than rejected: an origin has
	// none of them by definition, and the caller's server URL legitimately does
	// ("https://h/mcp" is a normal MCP endpoint). Discarding is the whole point
	// of the function, not a concession.
	origin := canonicalOrigin(strings.ToLower(u.Scheme), host, port)
	if len(origin) > MaxOriginBytes {
		return fail(fmt.Sprintf("origin is %d bytes, max %d", len(origin), MaxOriginBytes))
	}
	return origin, nil
}

// canonicalHost lowercases host, drops a trailing root label, and canonicalizes
// an IP literal. It rejects the spellings CanonicalOrigin refuses to guess at.
func canonicalHost(host string) (string, error) {
	if host == "" {
		return "", fmt.Errorf("URL has no host")
	}
	// Rejected before the trailing-dot trim so that the zone's own "%" cannot be
	// mistaken for anything else. Hostname() unescapes "%25" to "%", which
	// url.Parse then rejects on the way back in — so a zone cannot survive the
	// round trip even if we wanted it to.
	if strings.Contains(host, "%") {
		return "", fmt.Errorf("URL host must not carry an IPv6 zone identifier")
	}
	if i := strings.IndexFunc(host, func(r rune) bool { return r > 127 }); i >= 0 {
		return "", fmt.Errorf("URL host must be ASCII; encode an internationalized name as its punycode A-label")
	}

	host = strings.ToLower(host)
	// "example.com." names the same host as "example.com", with the root label
	// spelled out. Exactly one trailing dot is that spelling; "example.com.."
	// is not a name at all, so it falls through to the label check below.
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return "", fmt.Errorf("URL has no host")
	}

	// An IP literal is canonicalized by net.IP.String, which compresses IPv6 and
	// lowercases its hex — "[::0001]" and "[::1]" are one address, and keying
	// them separately would cost a redundant login.
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	if err := checkHostLabels(host); err != nil {
		return "", err
	}
	return host, nil
}

// checkHostLabels reports whether host is a plausible DNS name.
//
// This is a fail-closed check, not an RFC 1035 conformance test: url.Parse is
// permissive about what it will accept as a host, and this host is about to be
// concatenated into a Key that reaches log lines and into URLs the flow
// fetches. The permitted set is letters, digits, hyphen, dot and underscore —
// LDH plus the underscore that appears in real service names.
func checkHostLabels(host string) error {
	// Exactly one trailing dot is the root label and has already been trimmed
	// by the caller. Anything still touching a dot at either end is an empty
	// label: "example.com.." trims to "example.com.", which is not a name — and
	// letting it through would return an origin that Key.Validate then rejects,
	// breaking the invariant this whole file exists to uphold. That is not
	// hypothetical; the invariant test caught exactly this.
	if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return fmt.Errorf("URL host has an empty label")
	}
	if strings.Contains(host, "..") {
		return fmt.Errorf("URL host has an empty label")
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.', c == '_':
		default:
			// The byte is reported, not the host: a host is not secret, but a
			// malformed one is attacker-shaped and quoting it whole into a log
			// is how log-forging works. The index is enough to fix it.
			return fmt.Errorf("URL host has an invalid byte 0x%02x at index %d", c, i)
		}
	}
	return nil
}

// canonicalPort normalizes an explicit port to its minimal spelling, returning
// "" for the scheme's default port.
func canonicalPort(scheme, port string) (string, error) {
	if port == "" {
		return "", nil
	}
	// url.Parse guarantees the port is digits, but nothing more: it accepts
	// ":0", ":99999" and ":0443" alike.
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("URL has an invalid port")
	}
	// strconv.Itoa is what strips a leading zero: ":0443" and ":443" are the
	// same port, and comparing the string against "443" would miss it.
	switch scheme := strings.ToLower(scheme); {
	case scheme == "https" && n == 443, scheme == "http" && n == 80:
		return "", nil
	}
	return strconv.Itoa(n), nil
}
