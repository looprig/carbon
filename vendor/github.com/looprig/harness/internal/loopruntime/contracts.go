package loopruntime

import "github.com/looprig/harness/pkg/loop"

type (
	AccessGate             = loop.AccessGate
	ReadGuard              = loop.ReadGuard
	Delegation             = loop.Delegation
	RuntimeContextProvider = loop.RuntimeContextProvider
	Provenance             = loop.Provenance
	ConfigError            = loop.ConfigError
	ConfigErrorKind        = loop.ConfigErrorKind
	IDGenerationError      = loop.IDGenerationError
	CommitError            = loop.CommitError
	CommitCancelReason     = loop.CommitCancelReason
	Backend                = loop.Backend
)

const (
	ConfigMissingClient    = loop.ConfigMissingClient
	ConfigInvalidModel     = loop.ConfigInvalidModel
	ConfigMissingPublisher = loop.ConfigMissingPublisher
	CommitTurnCancelled    = loop.CommitTurnCancelled
	DelegationManaged      = loop.DelegationManaged
)

var (
	WithProvenance          = loop.WithProvenance
	ProvenanceFrom          = loop.ProvenanceFrom
	WithToolUseID           = loop.WithToolUseID
	ToolUseIDFrom           = loop.ToolUseIDFrom
	WithPreparedCall        = loop.WithPreparedCall
	PreparedCallFromContext = loop.PreparedCallFromContext
	WithUserInputRequester  = loop.WithUserInputRequester
	WithApprovalRequester   = loop.WithApprovalRequester
)
