// Package subscription exposes the OpenAI subscription registration policy
// boundary. It deliberately has no credential, transport, or discovery
// implementation: the current policy is to reject unsanctioned third-party
// registration attempts before they can become an inference flow.
package subscription

import (
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/looprig/llm"
)

// Status is the immutable state of a subscription registration gate.
type Status string

const (
	// StatusUnavailable means the provider has not published a sanctioned
	// third-party subscription-registration contract.
	StatusUnavailable Status = "unavailable"
	// StatusBlocked means callers must not proceed through this gate.
	StatusBlocked Status = "blocked"

	// RegistrationStatusUnavailable and RegistrationStatusBlocked are explicit
	// aliases for callers that prefer the longer enum names.
	RegistrationStatusUnavailable = StatusUnavailable
	RegistrationStatusBlocked     = StatusBlocked
)

// ReviewedAtDate is the date on which the official evidence for this gate was
// reviewed. It is a date, not a timestamp, so it has no local-time ambiguity.
const ReviewedAtDate = "2026-08-09"

// The evidence set is deliberately fixed and bounded. These are documentation
// references only; no value from them is used to construct a request.
const (
	EvidenceAuthURL                   = "https://learn.chatgpt.com/docs/auth"
	EvidenceCodexPlanURL              = "https://help.openai.com/en/articles/11369540-using-codex-with-your-chatgpt-plan"
	EvidenceEnterpriseAccessTokensURL = "https://learn.chatgpt.com/docs/enterprise/access-tokens" // #nosec G101 -- "access-tokens" is a documentation URL phrase, not a credential.
	EvidenceSignInWithChatGPTURL      = "https://help.openai.com/en/articles/20001410-sign-in-with-chatgpt"
	EvidenceAPIQuickstartURL          = "https://platform.openai.com/docs/quickstart/make-your-first-api-request"
)

const maxEvidenceURLs = 5

// RegistrationGate is an immutable, provider-specific registration policy.
// Its state is private so callers can only observe copies of the metadata.
// Require always fails closed for the current OpenAI subscription policy.
type RegistrationGate struct {
	status       Status
	provider     llm.Provider
	reviewedAt   time.Time
	evidenceURLs [maxEvidenceURLs]string
}

var (
	_ fmt.Formatter  = RegistrationGate{}
	_ slog.LogValuer = RegistrationGate{}
)

// OpenAIRegistration returns the current OpenAI subscription registration
// gate. Every call returns an independent value; its evidence accessor also
// returns a fresh copy.
func OpenAIRegistration() RegistrationGate {
	return RegistrationGate{
		status:     StatusBlocked,
		provider:   llm.ProviderOpenAI,
		reviewedAt: time.Date(2026, time.August, 9, 0, 0, 0, 0, time.UTC),
		evidenceURLs: [maxEvidenceURLs]string{
			EvidenceAuthURL,
			EvidenceCodexPlanURL,
			EvidenceEnterpriseAccessTokensURL,
			EvidenceSignInWithChatGPTURL,
			EvidenceAPIQuickstartURL,
		},
	}
}

// Status reports the gate's fail-closed status.
func (g RegistrationGate) Status() Status { return g.status }

// Provider reports the provider policy label covered by the gate.
func (g RegistrationGate) Provider() llm.Provider { return g.provider }

// ReviewedAt reports the UTC midnight at which the evidence was reviewed.
func (g RegistrationGate) ReviewedAt() time.Time { return g.reviewedAt }

// ReviewedDate reports the reviewed-at date in ISO-8601 calendar form.
func (g RegistrationGate) ReviewedDate() string {
	if g.reviewedAt.IsZero() {
		return ""
	}
	return g.reviewedAt.UTC().Format("2006-01-02")
}

// EvidenceURLs returns a bounded copy of the official evidence URL metadata.
func (g RegistrationGate) EvidenceURLs() []string {
	urls := make([]string, 0, maxEvidenceURLs)
	for _, raw := range g.evidenceURLs {
		if raw != "" {
			urls = append(urls, raw)
		}
	}
	return urls
}

// Format keeps diagnostics from reflecting private fields. The gate contains
// only bounded policy metadata today, but this formatter makes that boundary
// durable if the representation grows later.
func (g RegistrationGate) Format(state fmt.State, verb rune) {
	message := g.safeSummary()
	switch verb {
	case 'q':
		_, _ = io.WriteString(state, strconv.Quote(message))
	case 'x':
		_, _ = io.WriteString(state, hex.EncodeToString([]byte(message)))
	case 'X':
		_, _ = io.WriteString(state, strings.ToUpper(hex.EncodeToString([]byte(message))))
	default:
		_, _ = io.WriteString(state, message)
	}
}

// LogValue exposes only the safe gate state needed for policy diagnostics;
// evidence URLs and any future registration fields are intentionally omitted.
func (g RegistrationGate) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("status", string(g.status)),
		slog.String("provider", string(g.provider)),
		slog.String("reviewed_at", g.ReviewedDate()),
		slog.Int("evidence_count", len(g.EvidenceURLs())),
	)
}

func (g RegistrationGate) safeSummary() string {
	return fmt.Sprintf("openai subscription registration gate: status=%s provider=%s reviewed_at=%s evidence_count=%d", g.status, g.provider, g.ReviewedDate(), len(g.EvidenceURLs()))
}

// Require rejects use of this registration path. A zero-value gate also fails
// closed, so an uninitialized value cannot accidentally authorize a flow.
func (g RegistrationGate) Require() error {
	return &UnsupportedRegistrationError{}
}
