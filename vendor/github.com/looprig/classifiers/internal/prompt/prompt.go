// Package prompt holds the immutable system prompt for the gate.command-safety
// classifier. The prompt is embedded at build time and never mutated at
// runtime: its exact bytes are the classifier's identity-relevant policy text,
// folded into hustle definition identity via hustle.WithSystemPrompt, and
// never crosses this module's public boundary directly (see
// docs/plans/2026-07-27-permission-classifier-hustle-design.md §19.2).
package prompt

import _ "embed"

// CommandSafetyRevision independently versions the command-safety prompt's
// exact wording, per design §19.2: "Prompt, policy, schema, wire, and corpus
// revisions are independently explicit and included in classifier identity."
// Bump this whenever command_safety.md's meaning changes.
const CommandSafetyRevision = "command-safety-prompt/v3"

//go:embed command_safety.md
var commandSafetyText string

// CommandSafety returns the immutable gate.command-safety system prompt.
func CommandSafety() string {
	return commandSafetyText
}
