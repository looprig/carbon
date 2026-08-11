package permission

import "strings"

// DiagnosticCode classifies one non-fatal rule diagnostic.
type DiagnosticCode string

// DiagnosticAllowFamilyOutOfCatalog reports a manually authored,
// syntactically valid allow family whose token prefix is outside the
// consumer's automatic eligibility catalog. The rule remains authoritative;
// the consumer must surface the diagnostic. Deny families never warn.
const DiagnosticAllowFamilyOutOfCatalog DiagnosticCode = "allow_family_out_of_catalog"

// Diagnostic is one non-fatal finding produced while loading rules. It is
// reported separately from fatal file errors and never alters rule
// precedence.
type Diagnostic struct {
	Code      DiagnosticCode
	RuleIndex int    // index of the rule in the loaded file
	Message   string // bounded, non-secret description
}

// FamilyEligibility reports whether an allow family with the given literal
// token prefix belongs to the consumer's explicit automatic-proposal
// catalog. The catalog itself is product policy and is injected by the
// consumer; a nil predicate treats every allow family as out of catalog.
type FamilyEligibility func(tokens []string) bool

// diagnoseRules computes the non-fatal diagnostics for a loaded rule set.
func diagnoseRules(rules []Rule, eligible FamilyEligibility) []Diagnostic {
	var diagnostics []Diagnostic
	for index, rule := range rules {
		if rule.Class != ClassCommandInvokeFamily || rule.Effect != EffectAllow {
			continue
		}
		if eligible != nil && eligible(append([]string(nil), rule.Tokens...)) {
			continue
		}
		diagnostics = append(diagnostics, Diagnostic{
			Code:      DiagnosticAllowFamilyOutOfCatalog,
			RuleIndex: index,
			Message:   "allow family \"" + strings.Join(rule.Tokens, " ") + "\" is outside the automatic eligibility catalog",
		})
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}
