package loopruntime

import (
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	model "github.com/looprig/inference/model"
)

// StaleCompactionError reports the complete measurement identity that failed
// the actor's compare-and-swap. A stale proposal never mutates live state.
type StaleCompactionError struct {
	ExpectedBasis              event.ContextBasis
	ActualBasis                event.ContextBasis
	ExpectedModel              model.ModelKey
	ActualModel                model.ModelKey
	ExpectedRequestFingerprint [32]byte
	ActualRequestFingerprint   [32]byte
}

func (*StaleCompactionError) Error() string {
	return "loopruntime: compaction replacement measurement is stale"
}

type actorContextReplacement struct {
	tracker contextTracker
}

// prepareActorContextReplacement performs every fallible live-state check before
// the canonical terminal append. The returned plan applies without validation
// or I/O after CompactionCommitted is durable.
func prepareActorContextReplacement(
	state loopState,
	attempt compactionAttempt,
	success *compactionPreparedSuccess,
	settings contextAdmissionSettings,
) (actorContextReplacement, error) {
	actualBasis := state.contextTracker.currentBasis()
	actualModel := state.context.Model
	actualFingerprint := state.context.RequestFingerprint
	if success == nil || !state.hasContext || state.context.Basis != attempt.Basis || actualBasis != attempt.Basis ||
		actualModel != success.Model || actualFingerprint != success.RequestFingerprint {
		expectedModel := model.ModelKey{}
		expectedFingerprint := [32]byte{}
		if success != nil {
			expectedModel = success.Model
			expectedFingerprint = success.RequestFingerprint
		}
		return actorContextReplacement{}, &StaleCompactionError{
			ExpectedBasis: attempt.Basis, ActualBasis: actualBasis,
			ExpectedModel: expectedModel, ActualModel: actualModel,
			ExpectedRequestFingerprint: expectedFingerprint, ActualRequestFingerprint: actualFingerprint,
		}
	}
	tracker := contextTracker{}
	postContext, err := success.PostCount.measurement(attempt.Basis)
	if err != nil {
		return actorContextReplacement{}, err
	}
	if err := tracker.restore(attempt.Basis, true, postContext, true, event.ContextBasis{}, false, settings); err != nil {
		return actorContextReplacement{}, err
	}
	return actorContextReplacement{tracker: tracker}, nil
}

// apply projects the already-durable canonical replacement into actor memory.
// It deliberately leaves inbox/draining and all turn identity untouched.
//
// state.msgsDerivedPrefix is set to 1 here: committed.Summary is a
// model-generated compaction summary (the compaction Hustle's LLM call
// output, wrapped as a *content.UserMessage — never something a human
// typed), and it becomes the ENTIRE new state.msgs. Without this, the next
// turn's installActiveTurn would clone it into cfg.base with no marker at
// all, and capturePermissionReviewContext (review_context.go) would credit
// it with gate.ReviewContextOriginUser — genuine human authorization — for
// every LATER turn, or after a restore whose folded history begins with
// this exact summary (see loopState.msgsDerivedPrefix's own doc comment).
func (p actorContextReplacement) apply(state *loopState, committed event.CompactionCommitted) {
	tracker := p.tracker
	tracker.basis = committed.PostContext.Basis
	tracker.measurement = committed.PostContext
	tracker.hasMeasurement = true
	state.msgs = content.AgenticMessages{cloneUserMessage(committed.Summary)}
	state.msgsDerivedPrefix = 1
	state.context = committed.PostContext
	state.hasContext = true
	state.contextTracker = tracker
}

type turnContextReplacement struct {
	Summary *content.UserMessage
}

// applyTurnContextReplacement is the private turn-goroutine half of the actor
// handshake. Only request history changes; identity, usage, and tool counters do
// not.
//
// config.baseDerivedPrefix is reset to 0 alongside config.base: base is now
// empty, and a stale nonzero prefix from turn start would describe a slice
// that no longer exists (capturePermissionReviewContext's bounds check would
// then fail closed on every subsequent gate this turn, rather than simply
// reflecting that base's own derived content — if any — has been folded into
// state.msgs/state.derivedUserPrefix instead).
func applyTurnContextReplacement(config *turnConfig, state *turnState, replacement turnContextReplacement) {
	config.base = content.AgenticMessages{}
	config.baseDerivedPrefix = 0
	state.msgs = content.AgenticMessages{cloneUserMessage(replacement.Summary)}
	state.derivedUserPrefix = 1
}
