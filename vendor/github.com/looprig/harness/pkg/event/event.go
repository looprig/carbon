package event

import (
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
)

// Event is the sealed root of every loop event. Every concrete event embeds a
// Header, exactly one lifecycle mixin (ephemeral, enduring, or terminal), and
// exactly one scope mixin (sessionScoped or loopScoped). The lifecycle mixin
// supplies Class()/EndsTurn() and the scope mixin supplies Scope(), so the hub
// gets its delivery policy and consumers get producer identity without a
// transport-only envelope or a concrete type switch. Embedding two lifecycle
// mixins or two scope mixins makes the selectors ambiguous and the type stops
// satisfying Event, so the "exactly one of each" rule is enforced by the
// compiler.
type Event interface {
	isEvent()
	Class() Class
	Scope() Scope
	EndsTurn() bool // turn-terminal: the last event this turn's per-turn stream carries
	EventHeader() Header
	Visibility() EventVisibility
}

// Reply is an event that is the direct outcome of a command, delivered on the normal
// fan-in (classed Ephemeral/Enduring like any other event — NOT a point-to-point
// channel). It is the typed replacement for the command.Disposition reply: an issuer
// recognises "the answer to my command" via ReplyTo() == its command id.
type Reply interface {
	Event
	isReply()           // seals the set
	ReplyTo() uuid.UUID // == Header.Cause.CommandID: the command this answers
}

// ReplyTo returns the id of the command this event answers (its Cause.CommandID).
// It is promoted onto every Reply event via the embedded Header.
func (h Header) ReplyTo() uuid.UUID { return h.Cause.CommandID }

// Class is the delivery class of an event. It is semantic — "is this event
// reconstructable from a later authoritative event?" — not a transport flag,
// which is why it belongs on the event rather than on the transport.
type Class uint8

const (
	Ephemeral Class = iota // reconstructable from a later authoritative event -> droppable
	Enduring               // authoritative transition/payload -> never silently dropped
)

// Scope is whether an event is session-global or produced by one loop.
type Scope uint8

const (
	ScopeSession Scope = iota
	ScopeLoop
)

// EventVisibility controls whether an event may enter ordinary product streams.
// Public is deliberately zero so legacy records remain public and byte-stable.
type EventVisibility uint8

const (
	Public EventVisibility = iota
	Internal
)

// Valid reports whether visibility belongs to the closed durable domain.
func (v EventVisibility) Valid() bool { return v == Public || v == Internal }

// CancelReason explains why a queued input left the loop queue without
// committing. It is carried by InputCancelled.
type CancelReason uint8

const (
	CancelClientRetracted CancelReason = iota
	CancelTurnInterrupted
	CancelTurnFailed
)

// Header is the producer identity stamped on every event. The producer (the loop
// for loop events, the session for session events) fills it in; fan-in, filter,
// and journal consumers read it without a transport-only envelope.
type Header struct {
	// Coordinates is the producer's location in the hierarchy: SessionID on every
	// event; LoopID for loop-scoped events; TurnID for turn events; StepID for
	// step/tool scoped events. Session-scoped events leave LoopID/TurnID/StepID zero.
	identity.Coordinates

	// AgentName is the immutable attribution name of the agent driving the producing
	// loop, stamped at loop creation onto its LoopStarted (and carried on the loop's
	// other events via the same Header). It is empty (omitzero) for a plain loop and for
	// any record persisted before AgentName existed, so old journals stay byte-compatible
	// — the field serializes additively with no new codec case. Restore validates the
	// root loop's stamped name against the configured primary's name.
	AgentName identity.AgentName `json:"agent_name,omitzero"`

	// EventID identifies this event. Header carries this identity directly; detailed
	// wiring is sequenced after the journal follow-on.
	EventID uuid.UUID `json:"event_id,omitzero"`

	// CreatedAt is when this event was created (minted at creation, not delivery).
	// It is the journal's creation timestamp for every Enduring event.
	CreatedAt time.Time `json:"created_at,omitzero"`

	// Cause is the direct cause of this event. For UserInput/SubagentResult
	// resolution events (TurnStarted, TurnFoldedInto, InputCancelled, InputQueued,
	// TurnRejected), Cause.CommandID is the submit command id. For an event caused by
	// a SubagentResult, Cause.LoopID is the producing agent's loop id (its
	// quiescence wake token). Cause.Agency surfaces who caused it, but ONLY the turn-
	// resolution events stamp it — TurnStarted, TurnFoldedInto, and InputCancelled
	// (per design §444-446); InputQueued and TurnRejected carry Cause.CommandID but
	// NOT Cause.Agency.
	Cause identity.Cause `json:"cause,omitzero"`

	// EventVisibility is omitted for Public so pre-visibility journals remain a
	// byte-for-byte fixed point. Internal events always persist the non-zero tag.
	EventVisibility EventVisibility `json:"visibility,omitzero"`
}

// EventHeader returns the embedded Header so every event satisfies Event without
// per-type boilerplate.
func (h Header) EventHeader() Header { return h }

// Visibility returns the event's durable product-delivery classification.
func (h Header) Visibility() EventVisibility { return h.EventVisibility }

// Subscription is the consumer-facing handle to a session event fan-in: the
// read+teardown contract a TUI/CLI (or a future journal) depends on, independent
// of the concrete subscription implementation. It lives here in the (leaf) event
// package — the one package every event producer and consumer already imports — so
// neither side has to depend on the concrete hub type to name the contract
// (Dependency Inversion). The session hub's *EventSubscription satisfies it
// structurally.
//
// Events yields the filtered fan-in stream and closes on Close or on a hub-forced
// loss; Close is the consumer's intentional, idempotent teardown; Err reports the
// typed termination cause (nil for an intentional Close, the loss error for a
// hub-forced drop).
type Subscription interface {
	Events() <-chan Delivery
	Close() error
	Err() error
}

// Delivery is one fan-in delivery: the event plus its durable journal sequence.
// JournalSeq is 0 for Ephemeral deliveries (never persisted, never sequenced) and
// the strictly-monotonic append sequence for Enduring deliveries. It rides only the
// LIVE delivery path — it is never part of the persisted event codec, so the durable
// envelope stays byte-compatible.
type Delivery struct {
	Event      Event
	JournalSeq uint64
}

// ephemeral is the lifecycle mixin for a streaming delta: droppable, never
// turn-terminal.
type ephemeral struct{}

func (ephemeral) Class() Class   { return Ephemeral }
func (ephemeral) EndsTurn() bool { return false }

// enduring is the lifecycle mixin for an authoritative, mid-turn or mid-loop
// transition/payload: never silently dropped, never turn-terminal.
type enduring struct{}

func (enduring) Class() Class   { return Enduring }
func (enduring) EndsTurn() bool { return false }

// terminal is the lifecycle mixin for a turn-ender. It folds in Class()==Enduring,
// so terminal => Enduring holds by construction — a turn-ender can never be
// classified droppable.
type terminal struct{}

func (terminal) Class() Class   { return Enduring }
func (terminal) EndsTurn() bool { return true }

// sessionScoped is the scope mixin for a session-global event.
type sessionScoped struct{}

func (sessionScoped) Scope() Scope { return ScopeSession }

// loopScoped is the scope mixin for a loop-produced event.
type loopScoped struct{}

func (loopScoped) Scope() Scope { return ScopeLoop }

// HustleRunDescriptor is the secret-free identity and resolved runtime of one
// invocation. Started events carry a zero Runtime; terminal events carry the
// resolved runtime whenever model resolution succeeded.
type HustleRunDescriptor struct {
	Definition hustle.DefinitionDescriptor `json:"definition"`
	RunID      hustle.RunID                `json:"run_id"`
	Runtime    ModelRuntime                `json:"runtime,omitzero"`
}

// HustleStarted durably records ownership before scheduler eligibility.
type HustleStarted struct {
	enduring
	sessionScoped
	Header
	Run HustleRunDescriptor `json:"run"`
}

// HustleCompleted durably records one successful terminal outcome.
type HustleCompleted struct {
	enduring
	sessionScoped
	Header
	Run      HustleRunDescriptor `json:"run"`
	Duration time.Duration       `json:"duration,omitzero"`
	Usage    *content.Usage      `json:"usage,omitempty"`
}

// HustleFailed durably records one bounded, security-safe failure outcome.
type HustleFailed struct {
	enduring
	sessionScoped
	Header
	Run        HustleRunDescriptor `json:"run"`
	Duration   time.Duration       `json:"duration,omitzero"`
	Stage      hustle.Stage        `json:"stage"`
	ReasonCode hustle.ReasonCode   `json:"reason_code"`
	Usage      *content.Usage      `json:"usage,omitempty"`
}

// TurnIndex identifies a turn within one loop. Each loop numbers its own turns
// from 0; it is not unique across loops in a multi-loop session.
type TurnIndex int

// SessionStarted is published when the session's primary loop actor starts.
// Header.SessionID is set; LoopID/TurnID/StepID are zero. Config is the fingerprint
// of the agent configuration the session started under (model/system-prompt/tool
// policy), stamped at construction so a durable journal can detect a config change
// on restore.
//
// Manifest is the richer, canonical description of the same configuration. During
// the deprecation window BOTH are populated: Config remains the legacy comparison
// baseline while Manifest carries the additive fields (workspace trust, permission
// and confinement strictness, application fields, and per-tool schema identity) that
// typed drift assessment consumes. It is additive (omitzero): a journal record that
// predates the field decodes it as the zero ConfigManifest and never spuriously
// mismatches.
type SessionStarted struct {
	enduring
	sessionScoped
	Header
	Config   ConfigFingerprint `json:"config,omitzero"`
	Manifest ConfigManifest    `json:"manifest,omitzero"`
}

// SessionActive marks the Idle -> Active edge of the session quiescence model.
type SessionActive struct {
	enduring
	sessionScoped
	Header
}

// SessionIdle marks the Active -> Idle edge of the session quiescence model.
type SessionIdle struct {
	enduring
	sessionScoped
	Header
}

// SessionStopped marks the session phase transition on Shutdown.
type SessionStopped struct {
	enduring
	sessionScoped
	Header
}

// RestoreStarted marks the beginning of a session restore from the durable
// journal. Like SessionStarted it is session-scoped and Enduring (an
// authoritative session-lifecycle transition, never a turn-ender); Header.SessionID
// is set, LoopID/TurnID/StepID are zero.
type RestoreStarted struct {
	enduring
	sessionScoped
	Header
}

// RestoreDone marks a successful end of a session restore from the durable
// journal — the session is reconstructed and ready to resume. It is session-scoped
// and Enduring; Header.SessionID is set, LoopID/TurnID/StepID are zero.
type RestoreDone struct {
	enduring
	sessionScoped
	Header
}

// RestoreErrored marks a failed session restore from the durable journal. Err
// carries the typed cause; like TurnFailed.Err an error value cannot round-trip
// through encoding/json, so it is tagged json:"-" — callers read it in-memory via
// errors.As, and the durable codec's projection lands in a later phase. It is
// session-scoped and Enduring; Header.SessionID is set, LoopID/TurnID/StepID are
// zero.
type RestoreErrored struct {
	enduring
	sessionScoped
	Header
	// Err is the typed cause of the restore failure; tagged json:"-" because an
	// error value has no stable codec (mirrors TurnFailed.Err). Callers inspect it
	// in-memory via errors.As; a journal records the failure via the event itself.
	Err error `json:"-"`
}

// DecisionSource records who or what accepted a configuration adoption.
type DecisionSource string

const (
	DecisionSourceUser     DecisionSource = "user"
	DecisionSourcePolicy   DecisionSource = "policy"
	DecisionSourceOperator DecisionSource = "operator"
	// DecisionSourceMigration is stamped only by Harness itself when a Phase 2
	// migration adopts a configuration; a RestoreDecider never produces it.
	DecisionSourceMigration DecisionSource = "migration"
)

// Valid reports whether the source is one of the four closed DecisionSource
// values. An adoption record with any other source is malformed.
func (s DecisionSource) Valid() bool {
	switch s {
	case DecisionSourceUser, DecisionSourcePolicy, DecisionSourceOperator, DecisionSourceMigration:
		return true
	default:
		return false
	}
}

// ConfigurationAdopted commits a new configuration epoch: the durable record of
// an accepted restore drift or a one-time baseline upgrade. It is appended under
// the restore lease after the decision validates and before RestoreDone; the
// latest SessionStarted or ConfigurationAdopted is the baseline for the next
// restore's drift assessment. Like SessionStarted it is session-scoped and
// Enduring — Header.SessionID is set, LoopID/TurnID/StepID are zero, and the
// SessionID rides in the Header (there is no standalone SessionID field).
type ConfigurationAdopted struct {
	enduring
	sessionScoped
	Header
	Epoch               ConfigEpoch    `json:"epoch"`
	PreviousFingerprint string         `json:"previous_fingerprint,omitzero"`
	AdoptedFingerprint  string         `json:"adopted_fingerprint"`
	Manifest            ConfigManifest `json:"manifest"`
	Drift               []DriftChange  `json:"drift,omitzero"`
	Source              DecisionSource `json:"source"`
	Actor               string         `json:"actor,omitzero"`
	AppVersion          string         `json:"app_version,omitzero"`
	// Message is durable user-authored data, not an instruction: it never gains
	// authority during future prompt construction.
	Message string `json:"message,omitzero"`
}

// WorkspaceCheckpointed records that the session's workspace was durably
// snapshotted as Ref at this point in the event order. It is session-scoped and
// Enduring — the resume token's pointer to the workspace store; Header.SessionID
// is set, LoopID/TurnID/StepID are zero. Ref is an opaque "v1:sha256:<hex>" string
// (typed as workspacestore.Ref at the producer; pkg/event stays dependency-light).
type WorkspaceCheckpointed struct {
	enduring
	sessionScoped
	Header
	Ref         string              `json:"ref"`
	Consistency SnapshotConsistency `json:"consistency"`
	Trigger     SnapshotTriggerKind `json:"trigger"`
}

// SnapshotConsistency describes whether harness-managed workspace mutations could
// overlap a snapshot walk. Unknown exists only to decode legacy checkpoint events.
type SnapshotConsistency uint8

const (
	SnapshotConsistencyUnknown SnapshotConsistency = iota
	SnapshotQuiescent
	SnapshotFuzzy
)

// SnapshotTriggerKind records the policy boundary that requested a snapshot.
// Unknown exists only to decode legacy checkpoint events.
type SnapshotTriggerKind uint8

const (
	SnapshotTriggerKindUnknown SnapshotTriggerKind = iota
	SnapshotTriggerManual
	SnapshotTriggerIdle
	SnapshotTriggerInterrupt
	SnapshotTriggerTurnDone
	SnapshotTriggerStepDone
	SnapshotTriggerSeed
)

// WorkspaceRestored records that the live workspace was replaced from Ref and
// that Ref is now the effective durable restore point.
type WorkspaceRestored struct {
	enduring
	sessionScoped
	Header
	Ref string `json:"ref"`
}

// ActiveLoopChanged records the session's selected loop. The new selection is
// observable only after this session-scoped transition is durable.
type ActiveLoopChanged struct {
	enduring
	sessionScoped
	Header
	PreviousLoopID uuid.UUID `json:"previous_loop_id,omitzero"`
	ActiveLoopID   uuid.UUID `json:"active_loop_id"`
}

// LoopRestoreTombstoneCategory is the bounded reason a restored child was kept
// in the durable topology without a live backend.
const (
	LoopRestoreTombstoneRuntimeMismatch    = "runtime_mismatch"
	LoopRestoreTombstoneRuntimeUnavailable = "runtime_unavailable"
)

func ValidLoopRestoreTombstoneCategory(category string) bool {
	switch category {
	case LoopRestoreTombstoneRuntimeMismatch, LoopRestoreTombstoneRuntimeUnavailable:
		return true
	default:
		return false
	}
}

// LoopRestoreTombstoned records that a non-root child survived restore as a
// closed, failed registry entry because its durable runtime could not be
// re-authorized against the current runtime catalog.
type LoopRestoreTombstoned struct {
	enduring
	loopScoped
	Header
	Category string `json:"category"`
}

// LoopIdle is emitted when a loop parks with no active turn. Header.SessionID and
// Header.LoopID are set; TurnID/StepID are zero. It drives session quiescence.
type LoopIdle struct {
	enduring
	loopScoped
	Header
}

// LoopStarted is published by Session.NewLoop when a loop is registered. Header.Coordinates
// is the NEW loop (SessionID+LoopID set; TurnID/StepID zero). Header.Cause.Coordinates is the
// spawning loop/turn/step (zero for the primary = root); Cause.Agency = AgencyMachine. It is
// the durable loop-tree record for subscribers active at creation time.
type LoopStarted struct {
	enduring
	loopScoped
	Header
	// Runtime is the initial resolved model identity, limits, and effort. It is
	// durable so restore and catalog repair never consult a mutable catalog.
	Runtime      ModelRuntime  `json:"runtime,omitzero"`
	AgentRuntime *AgentRuntime `json:"agent_runtime,omitempty"`
	// ParentToolUseID is the durable provider tool-use id of the agent tool call
	// that spawned this loop (content.ToolUseBlock.ID), empty for loops not spawned by
	// a tool call (e.g. the primary/root). It is the durable carrier that correlates a
	// child loop back to its parent tool call across persist/restore; omitzero so old
	// journal records without the field decode to "".
	ParentToolUseID string `json:"parent_tool_use_id,omitzero"`
	// ForeignSID is the foreign agent's session id this loop is bound to, for
	// foreign-engine loops only; empty for native loops. It is the durable handle
	// used to --resume the foreign session across turns and across restore. omitzero
	// so old journal records (and native loops) decode to "". Mirrors
	// ParentToolUseID: identity metadata carried on the loop's start event.
	ForeignSID string `json:"foreign_sid,omitzero"`
	// InitialMode is the validated mode selected when the loop was constructed.
	// Empty identifies the base mode and preserves legacy records.
	InitialMode string `json:"initial_mode,omitzero"`
	// InitialRequestID proves the prepared delegate's initial command was accepted
	// before this durable loop-creation commit. Zero for roots/plain loops.
	InitialRequestID uuid.UUID `json:"initial_request_id,omitzero"`
	// DisplayName is the loop's user-facing presentation label, empty when the loop
	// declared none (consumers fall back to Header.AgentName). omitzero so old journal
	// records decode to "".
	DisplayName string `json:"display_name,omitzero"`
	// Description is the loop's user-facing description, empty when none declared.
	Description string `json:"description,omitzero"`
}

// AgentRuntime is the bounded, secret-free identity of the runtime selected for
// a loop. It is additive so legacy LoopStarted records decode with nil.
type AgentRuntime struct {
	Harness         string `json:"harness"`
	Profile         string `json:"profile"`
	CredentialMode  string `json:"credential_mode"`
	Source          string `json:"source,omitempty"`
	SelectionKind   string `json:"selection_kind,omitempty"`
	ModelAlias      string `json:"model_alias"`
	SmallModelAlias string `json:"small_model_alias,omitempty"`
	ACPSessionID    string `json:"acp_session_id,omitempty"`
}

// DelegateRequestAccepted is the durable actor-side acceptance of a follow-up
// machine NoFold request, emitted before it can queue or start.
type DelegateRequestAccepted struct {
	enduring
	loopScoped
	Header // Cause.CommandID=request, Coordinates.LoopID=target child
}

// ForeignSessionBound records the foreign agent session id for adapters that
// cannot accept a pre-minted id at LoopStarted time. It is loop-scoped and
// Enduring because restore needs it to resume the foreign session.
type ForeignSessionBound struct {
	enduring
	loopScoped
	Header
	ForeignSID string `json:"foreign_sid"`
}

// LoopAgentSessionBound records the durable foreign agent session binding for a loop.
type LoopAgentSessionBound struct {
	enduring
	loopScoped
	Header
	ACPSessionID string `json:"acp_session_id"`
}

func (SessionStarted) isEvent()               {}
func (SessionActive) isEvent()                {}
func (SessionIdle) isEvent()                  {}
func (SessionStopped) isEvent()               {}
func (HustleStarted) isEvent()                {}
func (HustleCompleted) isEvent()              {}
func (HustleFailed) isEvent()                 {}
func (RestoreStarted) isEvent()               {}
func (RestoreDone) isEvent()                  {}
func (RestoreErrored) isEvent()               {}
func (ConfigurationAdopted) isEvent()         {}
func (WorkspaceCheckpointed) isEvent()        {}
func (WorkspaceRestored) isEvent()            {}
func (ActiveLoopChanged) isEvent()            {}
func (LoopRestoreTombstoned) isEvent()        {}
func (LoopIdle) isEvent()                     {}
func (LoopStarted) isEvent()                  {}
func (DelegateRequestAccepted) isEvent()      {}
func (DelegateDeliveryStateChanged) isEvent() {}
func (ForeignSessionBound) isEvent()          {}
func (LoopAgentSessionBound) isEvent()        {}
