// This file constructs the two identities a catalog entry has, and validates
// the one the server chose.
//
// A server's raw name is a protocol identifier: it is whatever the server said,
// it goes back on the wire verbatim, and this module never rewrites it. A model
// name is a display and routing identifier: it is qualified by the binding so
// that two servers offering "search" are distinguishable, and sanitized so that
// it survives an inference provider's constraints on tool names.
//
// The two are connected by a lookup table (Generation.byModelName), never by
// parsing. That is a deliberate constraint from the design: a model name is
// lossy — sanitization maps many raw names onto one candidate, and truncation
// throws bytes away — so reparsing one to recover the raw name would be
// guesswork, and guessing which tool to invoke is the one mistake here that
// runs the wrong code on a live system.

package catalog

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxRawNameBytes bounds a server-supplied identifier: a tool or prompt name, a
// resource URI, a URI template.
//
// It is generous because it covers URIs as well as names, and its job is not to
// enforce taste — it is to stop a server from making an identifier into a
// memory or log-volume problem. An identifier is retained for the life of a
// generation, indexed in a map, rendered into names, warnings and telemetry, and
// echoed back on the wire.
const MaxRawNameBytes = 512

// MaxModelNameBytes bounds a model-facing tool name.
//
// 64 is the tightest constraint in common use across inference providers, so a
// name that fits it fits everywhere. The design requires names to "remain
// within inference-provider limits"; picking the minimum is what makes one
// catalog usable from any provider rather than only the one it was discovered
// under.
const MaxModelNameBytes = 64

// modelNamePrefix marks a name as belonging to an MCP binding rather than to a
// native host tool, so the two namespaces cannot collide.
const modelNamePrefix = "mcp__"

// modelNameSep separates the binding from the raw name.
const modelNameSep = "__"

// suffixSep introduces a disambiguation suffix.
const suffixSep = "_"

// suffixLen is the total width a disambiguation suffix costs: the separator
// plus the hex rendering of ShortDigestBytes.
const suffixLen = len(suffixSep) + 2*ShortDigestBytes

// validateRawName rejects a server-supplied identifier that cannot safely
// become one.
//
// The rules are about usability as an identifier, not about taste. An empty
// name cannot be routed to. An oversized one is a resource problem
// (MaxRawNameBytes). Invalid UTF-8 and control characters are rejected rather
// than repaired: this string is echoed into logs, telemetry and a terminal, and
// a name carrying an ANSI escape or a line break is an injection into whatever
// renders it. Repairing it would also silently make two distinct names equal,
// which is a routing hazard — so it fails closed instead.
func validateRawName(f Family, name string) error {
	defect := func(reason string) error {
		return &DefectError{Family: f, Reason: reason}
	}
	if name == "" {
		return defect("identifier is empty")
	}
	if len(name) > MaxRawNameBytes {
		return defect(fmt.Sprintf("identifier is %d bytes, max %d", len(name), MaxRawNameBytes))
	}
	if !utf8.ValidString(name) {
		return defect("identifier is not valid UTF-8")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return defect(fmt.Sprintf("identifier contains control character %#U", r))
		}
	}
	return nil
}

// assignModelNames fills in ModelName for every tool in the set, in place.
//
// tools must already be in canonical order and free of duplicate raw names
// (buildTools guarantees both). The assignment is a function of the binding and
// the whole tool set, which is why it happens here rather than per tool:
// collisions can only be seen from the set.
//
// The rule is:
//
//   - The candidate name is "mcp__<binding>__<sanitized raw name>".
//   - A candidate that fits MaxModelNameBytes and is unique in the set is used
//     as-is.
//   - Otherwise — too long, or shared with a sibling after sanitization — the
//     name is truncated to make room and a digest of the binding and the raw
//     name is appended. The digest is of the *raw* name, so the suffix
//     distinguishes exactly the thing sanitization made indistinguishable.
//
// Every member of a colliding group is suffixed, not just the losers. There is
// no principled winner — the server did not order them and the sort here is
// alphabetical, not meaningful — so picking one would hand a stable name to an
// arbitrary tool and move it the day a sibling was renamed. Suffixing the whole
// group makes the outcome symmetric, and no tool's name depends on which of its
// colliding siblings sorted first.
//
// A name is stable for as long as its collision group is. Adding a tool that
// collides with an existing one renames both; removing it renames the survivor
// back. That is inherent in resolving collisions at all, and it is safe here
// because a rename is a catalog change: it produces a new digest, a new
// generation, and an adoption at a safe boundary, rather than a silent
// substitution under a live turn.
// Normalization is a compatibility tolerance, so it is gated: normalize reports
// whether this binding's profile permits it. Qualification is not — every name
// is prefixed and binding-qualified whatever the profile says, because that is
// this module's own namespacing and not a concession to a server. What the
// tolerance covers is a raw name that does not survive qualification unchanged:
// one carrying characters an inference provider will not accept, or one long
// enough that the qualified form must be truncated to fit. Under a strict
// profile such a tool is rejected rather than renamed.
//
// It reports whether the tolerance was actually needed, which is not the same as
// whether it was permitted: a well-behaved server produces no normalization
// under any profile.
func assignModelNames(binding string, tools []Tool, normalize bool) (applied bool, err error) {
	candidates := make([]string, len(tools))
	counts := make(map[string]int, len(tools))
	for i, t := range tools {
		c := modelNamePrefix + sanitizeNamePart(binding) + modelNameSep + sanitizeNamePart(t.RawName)
		candidates[i] = c
		counts[c]++
	}

	assigned := make(map[string]struct{}, len(tools))
	for i := range tools {
		name := candidates[i]
		// A name that sanitization changed, or that must be cut to fit, or that
		// collides with a sibling only because of one of those two: each is a
		// raw name this module cannot show a model as it stands.
		lossy := sanitizeNamePart(tools[i].RawName) != tools[i].RawName ||
			len(name) > MaxModelNameBytes ||
			counts[name] > 1
		if lossy {
			if !normalize {
				return false, &DefectError{
					Family: FamilyTools,
					Reason: fmt.Sprintf("tool %q cannot be shown to a model under its own name, and this binding's compatibility profile does not tolerate normalizing it", tools[i].RawName),
				}
			}
			applied = true
		}
		if len(name) > MaxModelNameBytes || counts[name] > 1 {
			name = suffixedName(name, digestName(binding, tools[i].RawName))
		}
		if _, dup := assigned[name]; dup {
			// Only reachable when two raw names collide after sanitization AND
			// their short digests collide too. It is astronomically unlikely
			// rather than impossible, and the failure mode if it were ignored is
			// that one tool's calls silently route to another — so it is a hard
			// failure, not a warning.
			return false, &DefectError{
				Family: FamilyTools,
				Reason: fmt.Sprintf("tool %q cannot be given a unique model-facing name: %q is taken", tools[i].RawName, name),
			}
		}
		assigned[name] = struct{}{}
		tools[i].ModelName = name
	}
	return applied, nil
}

// suffixedName truncates name to leave room for a digest suffix and appends it.
//
// The truncation is a byte cut with no rune-boundary care, which is safe only
// because sanitizeNamePart has already reduced the string to single-byte
// characters. It is not safe on arbitrary input; nothing else may call this.
func suffixedName(name string, d Digest) string {
	keep := MaxModelNameBytes - suffixLen
	if keep < 0 {
		keep = 0
	}
	if len(name) > keep {
		name = name[:keep]
	}
	return name + suffixSep + d.Short()
}

// sanitizeNamePart reduces one component of a name to the character set that
// inference providers accept for a tool name: ASCII letters, digits, underscore
// and hyphen.
//
// Every other rune — including any multi-byte one — becomes a single
// underscore. The mapping is deliberately many-to-one and lossy; the digest
// suffix, not this function, is what keeps the result unambiguous.
func sanitizeNamePart(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
