package permission

import (
	"strings"
)

// bashrule.go implements the token-aware Bash rule families of the
// access-profile specification.
//
// A normalized command is split into shell segments at `&&`, `||`, `;`, `|`,
// `|&`, `&`, newline, and subshell boundaries, and every segment is matched
// independently. A token-prefix family (Bash(git log:*)) covers a segment
// only when the segment is a supported simple command whose leading bare
// literal tokens equal the family tokens exactly — token equality, never a
// normalized string prefix. Anything the conservative grammar cannot prove
// simple (redirection, command/process substitution, backticks, dynamic
// expansion, escapes, ambiguous quoting, comments, assignment prefixes, glob
// or tilde in command position) is classified unsupported and can be covered
// only by a wildcard rule or an exact rule for the identical text; an
// unbalanced construct makes the whole command unsegmentable and only a
// wildcard or whole-command exact rule applies. Everything fails closed.

// shellToken is one word of a tokenized shell segment. Bare reports the word
// was written entirely as plain unquoted characters; only bare tokens may
// participate in family leading-token equality.
type shellToken struct {
	Text string
	Bare bool
}

// shellSegment is one independently matched shell segment. Raw is the
// trimmed original text of the segment; Tokens is populated only for
// supported simple segments.
type shellSegment struct {
	Raw       string
	Tokens    []shellToken
	Supported bool
}

// maxSubshellDepth bounds subshell recursion so hostile nesting cannot
// exhaust the stack. Deeper input is unsegmentable.
const maxSubshellDepth = 16

// splitShellCommand splits a normalized command into its independently
// matched shell segments. ok is false when the command cannot be segmented
// at all (unbalanced quote, backtick, or parenthesis; a backslash escape,
// which can hide a separator; or a control byte other than tab/newline);
// then only whole-command matching applies.
func splitShellCommand(command string) ([]shellSegment, bool) {
	if strings.ContainsFunc(command, func(r rune) bool {
		return (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f
	}) {
		return nil, false
	}
	return splitRegion(command, 0)
}

// splitRegion splits one parenthesis-free region into segments, splicing
// subshell contents recursively.
func splitRegion(text string, depth int) ([]shellSegment, bool) {
	if depth > maxSubshellDepth {
		return nil, false
	}
	var segments []shellSegment
	start := 0
	// emit appends the chunk [start, end) as one or more segments. An empty
	// middle chunk is an unsupported segment; separator handling decides
	// whether an empty final chunk is emitted at all.
	emit := func(end int) bool {
		raw := strings.TrimSpace(text[start:end])
		if raw == "" {
			segments = append(segments, shellSegment{Raw: ""})
			return true
		}
		if raw[0] == '(' {
			if closing := skipParens(raw, 0); closing == len(raw) {
				inner, ok := splitRegion(raw[1:len(raw)-1], depth+1)
				if !ok {
					return false
				}
				segments = append(segments, inner...)
				return true
			}
			// A parenthesis group with surrounding text in the same segment
			// is not a plain subshell; fail closed as unsupported.
			segments = append(segments, shellSegment{Raw: raw})
			return true
		}
		segments = append(segments, tokenizeSegment(raw))
		return true
	}

	i := 0
	for i < len(text) {
		switch c := text[i]; c {
		case '\'', '"', '`':
			end := skipQuoted(text, i)
			if end < 0 {
				return nil, false
			}
			i = end
		case '\\':
			// An escape can hide a separator from this splitter; the whole
			// command is unsegmentable.
			return nil, false
		case '(':
			end := skipParens(text, i)
			if end < 0 {
				return nil, false
			}
			i = end
		case ')':
			return nil, false
		case '$':
			if i+1 < len(text) && text[i+1] == '(' {
				end := skipParens(text, i+1)
				if end < 0 {
					return nil, false
				}
				i = end
			} else {
				i++
			}
		case '&', '|', ';', '\n':
			operator := text[i : i+1]
			if c == '&' && i+1 < len(text) && text[i+1] == '&' {
				operator = "&&"
			}
			if c == '|' && i+1 < len(text) && (text[i+1] == '|' || text[i+1] == '&') {
				operator = text[i : i+2]
			}
			if !emit(i) {
				return nil, false
			}
			i += len(operator)
			start = i
			if operator == "&&" || operator == "||" || operator == "|" || operator == "|&" {
				// These operators require a following command; an absent one
				// is emitted as an empty unsupported segment below.
				if strings.TrimSpace(text[start:]) == "" && !strings.ContainsAny(text[start:], "&|;\n") {
					segments = append(segments, shellSegment{Raw: ""})
					start = len(text)
					i = len(text)
				}
			}
		default:
			i++
		}
	}
	if start < len(text) || len(segments) == 0 {
		if strings.TrimSpace(text[start:]) == "" && len(segments) > 0 {
			return segments, true // trailing `;`, `&`, or newline
		}
		if strings.TrimSpace(text) == "" && len(segments) == 0 {
			return nil, true // empty command: zero segments
		}
		if !emit(len(text)) {
			return nil, false
		}
	}
	return segments, true
}

// skipQuoted returns the index just past the quoted region opened at
// text[open] ('\” , '"', or '`'), or -1 when unterminated. The region is
// treated as opaque here; the tokenizer decides supportability.
func skipQuoted(text string, open int) int {
	quote := text[open]
	for i := open + 1; i < len(text); i++ {
		if text[i] == quote {
			return i + 1
		}
	}
	return -1
}

// skipParens returns the index just past the parenthesis group opened at
// text[open], honoring nesting and quoted regions, or -1 when unbalanced or
// when an escape makes the extent ambiguous.
func skipParens(text string, open int) int {
	depth := 0
	i := open
	for i < len(text) {
		switch text[i] {
		case '\'', '"', '`':
			end := skipQuoted(text, i)
			if end < 0 {
				return -1
			}
			i = end
		case '\\':
			return -1
		case '(':
			depth++
			i++
		case ')':
			depth--
			i++
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return -1
}

// commandPositionUnsafe lists bytes that make the command-position token of
// a segment unsupported even when parsed cleanly: glob and brace expansion,
// tilde expansion, environment-assignment prefixes, and history expansion
// all select or synthesize a different executable.
const commandPositionUnsafe = "*?[]{}~=!"

// tokenizeSegment tokenizes one operator-free segment and classifies it.
// Unsupported constructs leave Tokens nil and Supported false; the raw text
// remains available for exact whole-segment coverage.
func tokenizeSegment(raw string) shellSegment {
	segment := shellSegment{Raw: raw}
	var tokens []shellToken
	i := 0
	for i < len(raw) {
		if raw[i] == ' ' || raw[i] == '\t' {
			i++
			continue
		}
		var text strings.Builder
		bare := true
		started := false
	word:
		for i < len(raw) {
			switch c := raw[i]; c {
			case ' ', '\t':
				break word
			case '\'':
				end := skipQuoted(raw, i)
				if end < 0 {
					return segment
				}
				text.WriteString(raw[i+1 : end-1])
				bare = false
				started = true
				i = end
			case '"':
				end := skipQuoted(raw, i)
				if end < 0 {
					return segment
				}
				body := raw[i+1 : end-1]
				if strings.ContainsAny(body, "$`\\") {
					return segment // expansion inside double quotes
				}
				text.WriteString(body)
				bare = false
				started = true
				i = end
			case '\\', '$', '`', '<', '>', '(', ')', ';', '&', '|', '\n':
				return segment // substitution, redirection, escape, leftover operator
			case '#':
				if !started {
					return segment // comment start hides the rest of the line
				}
				text.WriteByte(c)
				i++
			default:
				text.WriteByte(c)
				started = true
				i++
			}
		}
		tokens = append(tokens, shellToken{Text: text.String(), Bare: bare})
	}
	if len(tokens) == 0 {
		return segment
	}
	command := tokens[0]
	if !command.Bare || command.Text == "" || strings.ContainsAny(command.Text, commandPositionUnsafe) {
		return segment
	}
	segment.Tokens = tokens
	segment.Supported = true
	return segment
}

// familyCoversSegment reports whether one family rule covers one segment:
// the segment is supported and its leading tokens are bare literals equal to
// the family tokens, with arbitrary trailing argument tokens.
func familyCoversSegment(rule Rule, segment shellSegment) bool {
	if !segment.Supported || len(segment.Tokens) < len(rule.Tokens) || len(rule.Tokens) == 0 {
		return false
	}
	for i, want := range rule.Tokens {
		token := segment.Tokens[i]
		if !token.Bare || token.Text != want {
			return false
		}
	}
	return true
}

// familyMatchesCommand reports whether one family rule alone matches a whole
// normalized command: the command segments cleanly and every segment is
// covered by this family. A family never crosses a segment boundary, so
// Bash(git log:*) can never authorize "git log; rm -rf output".
func familyMatchesCommand(rule Rule, command string) bool {
	segments, ok := splitShellCommand(command)
	if !ok || len(segments) == 0 {
		return false
	}
	for _, segment := range segments {
		if !familyCoversSegment(rule, segment) {
			return false
		}
	}
	return true
}

// ruleCoversSegment reports whether one command rule covers one segment: a
// wildcard covers any segment, an exact rule covers the segment whose raw
// text is identical to its command, and a family covers its family segments.
func ruleCoversSegment(rule Rule, segment shellSegment) bool {
	switch rule.Class {
	case ClassCommandInvokeWildcard:
		return true
	case ClassCommandInvoke:
		return segment.Raw != "" && rule.Command == segment.Raw
	case ClassCommandInvokeFamily:
		return familyCoversSegment(rule, segment)
	}
	return false
}

// commandAllowCovered reports whether the allow rules cover a whole command:
// a wildcard or a whole-command exact rule covers everything; otherwise the
// command must segment cleanly and every segment must be covered by some
// allow rule.
func commandAllowCovered(rules []Rule, command string) bool {
	applicable := commandRules(rules, EffectAllow)
	for _, rule := range applicable {
		if rule.Class == ClassCommandInvokeWildcard {
			return true
		}
		if rule.Class == ClassCommandInvoke && rule.Command == command {
			return true
		}
	}
	segments, ok := splitShellCommand(command)
	if !ok || len(segments) == 0 {
		return false
	}
	for _, segment := range segments {
		covered := false
		for _, rule := range applicable {
			if ruleCoversSegment(rule, segment) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

// commandDenyMatched reports whether any deny rule matches the whole command
// or any one of its segments. Deny tightens per segment: a deny family for
// one segment rejects the whole compound command.
func commandDenyMatched(rules []Rule, command string) bool {
	applicable := commandRules(rules, EffectDeny)
	if len(applicable) == 0 {
		return false
	}
	for _, rule := range applicable {
		if rule.Class == ClassCommandInvokeWildcard {
			return true
		}
		if rule.Class == ClassCommandInvoke && rule.Command == command {
			return true
		}
	}
	segments, ok := splitShellCommand(command)
	if !ok {
		return false
	}
	for _, segment := range segments {
		for _, rule := range applicable {
			if ruleCoversSegment(rule, segment) {
				return true
			}
		}
	}
	return false
}

// commandRules selects the command-execution rules with the wanted effect.
func commandRules(rules []Rule, effect Effect) []Rule {
	var applicable []Rule
	for _, rule := range rules {
		if rule.Effect == effect && rule.Capability == CapabilityCommandExecute {
			applicable = append(applicable, rule)
		}
	}
	return applicable
}

// bashRuleSyntaxPrefix is the single source of truth for the Bash(...)
// display-rule namespace. Every parser arm that interprets display syntax
// (commandCandidateRule) and every guard that refuses commands colliding
// with it (collidesWithBashRuleSyntax) MUST derive from this constant. A
// new display syntax must extend the predicate before any parser learns to
// read it; commandCandidateRule treats the predicate's complement as the
// exact-command arm by construction, so an unguarded new namespace cannot
// be laundered through an exact candidate.
const bashRuleSyntaxPrefix = "Bash("

// collidesWithBashRuleSyntax reports whether a literal command string lives
// in the display-rule namespace that commandCandidateRule parses. Such a
// command must never be offered or accepted as a reusable exact candidate:
// the store would re-read it as a wildcard or family rule (or reject the
// batch), never as the exact command it is.
func collidesWithBashRuleSyntax(command string) bool {
	return strings.HasPrefix(command, bashRuleSyntaxPrefix)
}

// ProposeCommandCandidate returns the reusable command-candidate match
// string Bash preparation should display for one normalized command: a
// family candidate "Bash(tokens:*)" when the command is a single supported
// simple segment whose longest bare literal token prefix is in the injected
// eligibility catalog, and the exact normalized command otherwise. Unknown
// prefixes, shells, interpreters, execution wrappers, multi-segment
// commands, and unsupported syntax all fall back to the exact command
// because the positive catalog decides; bare Bash(*) is never proposed.
//
// A command whose own text collides with the Bash(...) rule-syntax namespace
// (see collidesWithBashRuleSyntax) gets NO reusable candidate — the empty
// string. An exact fallback for such a command would be re-read by the store
// as a wildcard or family rule, so a malicious literal command `Bash(*)`
// (a mere shell syntax error when run) could otherwise be laundered into a
// durable allow-everything record via `Approve always`. Refusal is chosen
// over an escaped encoding because the candidate Match doubles as the exact
// display text; once-only approval is unaffected, and a user who truly wants
// a durable exact rule for such a command can author the structured
// command.invoke.v1 file record, which is unambiguous.
func ProposeCommandCandidate(command string, eligible FamilyEligibility) string {
	exact := func() string {
		if collidesWithBashRuleSyntax(command) {
			return ""
		}
		return command
	}
	if eligible == nil {
		return exact()
	}
	segments, ok := splitShellCommand(command)
	if !ok || len(segments) != 1 || !segments[0].Supported {
		return exact()
	}
	tokens := segments[0].Tokens
	bare := 0
	for bare < len(tokens) && tokens[bare].Bare {
		bare++
	}
	for n := bare; n >= 1; n-- {
		prefix := make([]string, n)
		for i := range prefix {
			prefix[i] = tokens[i].Text
		}
		if validateFamilyTokens(prefix) != nil {
			continue
		}
		if eligible(prefix) {
			return "Bash(" + strings.Join(prefix, " ") + ":*)"
		}
	}
	return exact()
}

// parseFamilyCandidateTokens parses the body of a Bash(tokens:*) candidate
// with the real tokenizer: exactly one supported segment of bare literal
// tokens in canonical single-space form. Anything else — ambiguous quoting,
// metacharacters, operators, denormalized spacing — is rejected rather than
// interpreted as a raw prefix.
func parseFamilyCandidateTokens(body string) ([]string, error) {
	fail := func(reason string) ([]string, error) {
		return nil, &RuleError{Index: -1, Reason: "family candidate rejected: " + reason}
	}
	segments, ok := splitShellCommand(body)
	if !ok || len(segments) != 1 || !segments[0].Supported {
		return fail("tokens are not one supported simple segment")
	}
	texts := make([]string, len(segments[0].Tokens))
	for i, token := range segments[0].Tokens {
		if !token.Bare {
			return fail("token " + token.Text + " is not a bare literal")
		}
		texts[i] = token.Text
	}
	if err := validateFamilyTokens(texts); err != nil {
		return fail(err.Error())
	}
	if strings.Join(texts, " ") != body {
		return fail("tokens are not in canonical single-space form")
	}
	return texts, nil
}
