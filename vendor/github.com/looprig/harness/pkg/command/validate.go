package command

import (
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
)

// Rule is the human-readable invariant a CommandValidationError records, so the
// caller learns WHY a field is wrong, not just which.
type Rule string

const (
	// RuleRequired: the field must be non-zero for this command.
	RuleRequired Rule = "must be set"
	RuleInvalid  Rule = "is invalid"
)

// Command/field names for CommandValidationError. The CommandName/CommandField
// types are reused from command.go (the existing InvalidCommandError vocabulary).
const (
	CommandUserInput         CommandName = "UserInput"
	CommandSubagentResult    CommandName = "SubagentResult"
	CommandCancelQueuedInput CommandName = "CancelQueuedInput"
	CommandApproveToolCall   CommandName = "ApproveToolCall"
	CommandDenyToolCall      CommandName = "DenyToolCall"
	CommandProvideUserInput  CommandName = "ProvideUserInput"
	CommandCompact           CommandName = "Compact"
	CommandUnknown           CommandName = "Command"

	FieldCommandID             CommandField = "CommandID"
	FieldSessionID             CommandField = "SessionID"
	FieldLoopID                CommandField = "LoopID"
	FieldTargetCommandID       CommandField = "TargetCommandID"
	FieldTargetLoopID          CommandField = "TargetLoopID"
	FieldBackgroundHandBack    CommandField = "BackgroundHandBack"
	FieldDelegateDeliveryPhase CommandField = "DelegateDeliveryPhase"
	FieldToolExecutionID       CommandField = "ToolExecutionID"
	FieldAgency                CommandField = "Agency"
	FieldAction                CommandField = "Action"
)

// CommandValidationError reports that a command violates the ID fill matrix: Field
// names the offending identity/addressing field and Rule says why (required). It is
// a typed package-API error so a journal/test can errors.As it to inspect the exact
// violation rather than parse a string. It is distinct from InvalidCommandError
// (which guards required-channel contracts like Interrupt.Ack) so the two failure
// modes never alias at a call site.
type CommandValidationError struct {
	Command CommandName
	Field   CommandField
	Rule    Rule
}

func (e *CommandValidationError) Error() string {
	return "command: invalid " + string(e.Command) + ": " + string(e.Field) + " " + string(e.Rule)
}

// ValidateCommand checks cmd against the ID fill matrix and returns a typed
// *CommandValidationError on the first violation, nil when cmd satisfies every
// invariant. CommandID is required on every command; per-type addressing rules then
// apply (UserInput's machine delegate target/phase marker, SubagentResult/
// CancelQueuedInput coordinates, and the gate-reply GateRoute).
// Interrupt and Shutdown are session-wide control commands with no addressing today,
// so they are not validated here (their Ack-channel contract is checked by their own
// Validate). Fail-secure: a command type outside the addressed set passes only the
// universal CommandID check.
func ValidateCommand(cmd Command) error {
	h := cmd.CommandHeader()
	if h.CommandID.IsZero() {
		return &CommandValidationError{Command: commandName(cmd), Field: FieldCommandID, Rule: RuleRequired}
	}
	switch c := cmd.(type) {
	case UserInput:
		if c.BackgroundHandBack && c.Agency != identity.AgencyMachine {
			return &CommandValidationError{Command: CommandUserInput, Field: FieldBackgroundHandBack, Rule: RuleInvalid}
		}
		if !c.DelegateDeliveryPhase.Valid() && c.DelegateDeliveryPhase != "" {
			return &CommandValidationError{Command: CommandUserInput, Field: FieldDelegateDeliveryPhase, Rule: RuleInvalid}
		}
		if c.DelegateDeliveryPhase != "" && c.Agency != identity.AgencyMachine {
			return &CommandValidationError{Command: CommandUserInput, Field: FieldDelegateDeliveryPhase, Rule: RuleInvalid}
		}
		if (c.NoFold && c.Agency == identity.AgencyMachine) || c.BackgroundHandBack || c.DelegateDeliveryPhase != "" {
			if c.TargetLoopID.IsZero() {
				return &CommandValidationError{Command: CommandUserInput, Field: FieldTargetLoopID, Rule: RuleRequired}
			}
		}
		if c.BackgroundHandBack && !c.NoFold && c.DelegateDeliveryPhase == "" {
			return &CommandValidationError{Command: CommandUserInput, Field: FieldBackgroundHandBack, Rule: RuleInvalid}
		}
		return nil
	case SubagentResult:
		return validateSubagentResult(c)
	case CancelQueuedInput:
		return validateCancelQueuedInput(c)
	case CancelDelegateRequest:
		return validateCancelDelegateRequest(c)
	case Compact:
		return validateCompact(c)
	case ApproveToolCall:
		if err := validateGateRoute(CommandApproveToolCall, c.GateRoute); err != nil {
			return err
		}
		return validateApproveAction(c.Action)
	case DenyToolCall:
		return validateGateRoute(CommandDenyToolCall, c.GateRoute)
	case ProvideUserInput:
		return validateGateRoute(CommandProvideUserInput, c.GateRoute)
	case ProcessNotification:
		return validateProcessNotification(c)
	default:
		// UserInput, Interrupt, Shutdown, and any other command: only CommandID is
		// required (already checked above).
		return nil
	}
}

func validateCompact(c Compact) error {
	if c.SessionID.IsZero() {
		return &CommandValidationError{Command: CommandCompact, Field: FieldSessionID, Rule: RuleRequired}
	}
	if c.LoopID.IsZero() {
		return &CommandValidationError{Command: CommandCompact, Field: FieldLoopID, Rule: RuleRequired}
	}
	if c.Agency != identity.AgencyMachine && c.Agency != identity.AgencyUser {
		return &CommandValidationError{Command: CommandCompact, Field: FieldAgency, Rule: RuleInvalid}
	}
	return nil
}

// validateSubagentResult requires the embedded Coordinates.LoopID — the PARENT loop
// the hand-back is dispatched to (the session routes by loops[Coordinates.LoopID]).
func validateSubagentResult(c SubagentResult) error {
	if c.LoopID.IsZero() {
		return &CommandValidationError{Command: CommandSubagentResult, Field: FieldLoopID, Rule: RuleRequired}
	}
	return nil
}

// validateCancelQueuedInput requires the dispatch addressing (SessionID + LoopID)
// and the TargetCommandID naming the queued submit to retract.
func validateCancelQueuedInput(c CancelQueuedInput) error {
	if c.SessionID.IsZero() {
		return &CommandValidationError{Command: CommandCancelQueuedInput, Field: FieldSessionID, Rule: RuleRequired}
	}
	if c.LoopID.IsZero() {
		return &CommandValidationError{Command: CommandCancelQueuedInput, Field: FieldLoopID, Rule: RuleRequired}
	}
	if c.TargetCommandID.IsZero() {
		return &CommandValidationError{Command: CommandCancelQueuedInput, Field: FieldTargetCommandID, Rule: RuleRequired}
	}
	return nil
}

func validateCancelDelegateRequest(c CancelDelegateRequest) error {
	if c.SessionID.IsZero() {
		return &CommandValidationError{Command: CommandCancelDelegateRequest, Field: FieldSessionID, Rule: RuleRequired}
	}
	if c.LoopID.IsZero() {
		return &CommandValidationError{Command: CommandCancelDelegateRequest, Field: FieldLoopID, Rule: RuleRequired}
	}
	if c.TargetCommandID.IsZero() {
		return &CommandValidationError{Command: CommandCancelDelegateRequest, Field: FieldTargetCommandID, Rule: RuleRequired}
	}
	return nil
}

// validateProcessNotification requires the generic envelope CommandID to
// exactly equal the wrapped DTO's own stable CommandID (Task 4's contract:
// Harness never mints a replacement id) and delegates the closed
// coordinates/handle/state-reason validation to the DTO's own Validate — the
// SAME bounded/enum-only checks Task 4 already built, so the command layer
// never re-derives (and risks drifting from) that invariant set.
func validateProcessNotification(c ProcessNotification) error {
	if c.Notification.CommandID.IsZero() || c.Notification.CommandID != c.Header.CommandID {
		return &CommandValidationError{Command: CommandProcessNotification, Field: FieldNotification, Rule: RuleInvalid}
	}
	return c.Notification.Validate()
}

// validateApproveAction requires exactly one of the two approve actions.
// gate.ApprovalDeny travels on DenyToolCall, so it is rejected here alongside
// unknown and empty actions — fail-secure: a record that does not name a valid
// approval can never decode into one.
func validateApproveAction(action gate.ApprovalAction) error {
	switch action {
	case gate.ApprovalApprove, gate.ApprovalApproveAlwaysWorkspace:
		return nil
	default:
		return &CommandValidationError{Command: CommandApproveToolCall, Field: FieldAction, Rule: RuleInvalid}
	}
}

// validateGateRoute requires a gate reply's GateRoute to carry a non-zero LoopID
// (dispatch target) and ToolExecutionID (the gate match key).
func validateGateRoute(name CommandName, r GateRoute) error {
	if r.LoopID.IsZero() {
		return &CommandValidationError{Command: name, Field: FieldLoopID, Rule: RuleRequired}
	}
	if r.ToolExecutionID.IsZero() {
		return &CommandValidationError{Command: name, Field: FieldToolExecutionID, Rule: RuleRequired}
	}
	return nil
}

// commandName returns the concrete command type name for a CommandValidationError.
func commandName(cmd Command) CommandName {
	switch cmd.(type) {
	case UserInput:
		return CommandUserInput
	case SubagentResult:
		return CommandSubagentResult
	case CancelQueuedInput:
		return CommandCancelQueuedInput
	case CancelDelegateRequest:
		return CommandCancelDelegateRequest
	case ApproveToolCall:
		return CommandApproveToolCall
	case DenyToolCall:
		return CommandDenyToolCall
	case ProvideUserInput:
		return CommandProvideUserInput
	case Compact:
		return CommandCompact
	case Interrupt:
		return CommandInterrupt
	case Shutdown:
		return CommandShutdown
	case ProcessNotification:
		return CommandProcessNotification
	default:
		return CommandUnknown
	}
}
