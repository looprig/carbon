// gemini.go implements GeminiAdapter: the Gemini CLI harness adapter's
// gateway environment contract. Unlike ClaudeConnector and CodexConnector,
// this is deliberately NOT an ACP connector -- it is a bare HarnessAdapter,
// env-var construction only. Per the design doc's "Gemini CLI connector"
// section: "None of the surveyed hosts drive Gemini CLI over ACP, so there
// is no proven adapter contract to pin ... its ACP connector ships only
// after an adapter path is verified against a real Gemini CLI release."
// This file must never grow a GeminiConnector type that dials/manages a
// Gemini CLI ACP subprocess, claim a supported adapter executable/spawn
// contract, or add session/config-option handling -- that is scope creep
// against an explicit, deliberate design decision. See gemini_test.go's
// regression guard.
package launch

import "github.com/looprig/acp/transport/stdio"

const (
	// envGoogleGeminiBaseURL and envGeminiAPIKey point the Gemini CLI at
	// the model proxy binding instead of a real provider endpoint, in
	// Gemini API-key mode. Nothing Vertex-AI- or Code-Assist-related is
	// ever set: there is nothing to actively disable for those modes,
	// only nothing to add beyond these three variables (see the design
	// doc).
	envGoogleGeminiBaseURL = "GOOGLE_GEMINI_BASE_URL"
	// #nosec G101 -- this is the NAME of an environment variable, not a
	// credential value; the real secret is a runtime ProxyBinding token,
	// never a source-code literal.
	envGeminiAPIKey = "GEMINI_API_KEY"
	// envGeminiModel selects the harness-facing model alias.
	envGeminiModel = "GEMINI_MODEL"
)

// GeminiAdapter is a HarnessAdapter for the Gemini CLI harness. Construct
// with Gemini.
type GeminiAdapter struct {
	// Model is the harness-facing model alias GEMINI_MODEL is set to.
	// Required.
	Model string
}

// Gemini constructs a GeminiAdapter for model.
func Gemini(model string) *GeminiAdapter {
	return &GeminiAdapter{Model: model}
}

// Configure sets exactly GOOGLE_GEMINI_BASE_URL, GEMINI_API_KEY, and
// GEMINI_MODEL -- Gemini API-key mode -- and nothing else. cmd.Path is
// validated the same way every other Configure in this package validates
// its child's executable path (see cleanAbsolutePath); this package does
// not otherwise claim to know what that executable's own argument
// contract looks like, so Args is left exactly as the caller supplied it
// (copied defensively by buildChildCommand, never cleared or replaced the
// way ClaudeConnector/CodexConnector's Configure do for their own pinned
// adapters). cmd is never mutated; the returned Command is always a fresh
// copy.
func (g *GeminiAdapter) Configure(cmd stdio.Command, binding ProxyBinding) (stdio.Command, error) {
	if !cleanAbsolutePath(cmd.Path) {
		return stdio.Command{}, &PathError{Field: "Path", Reason: "must be a clean absolute path"}
	}
	if g.Model == "" {
		return stdio.Command{}, &ConfigError{Reason: "GeminiAdapter.Model is required"}
	}

	return buildChildCommand(cmd, []envOverride{
		{Key: envGoogleGeminiBaseURL, Value: binding.BaseURL},
		{Key: envGeminiAPIKey, Value: binding.Token},
		{Key: envGeminiModel, Value: g.Model},
	}, nil)
}

// Compile-time proof that GeminiAdapter actually satisfies HarnessAdapter.
var _ HarnessAdapter = (*GeminiAdapter)(nil)
