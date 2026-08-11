package event

import "github.com/looprig/harness/pkg/tool"

// ProcessStarted durably records that a supervised process reached running.
type ProcessStarted struct {
	enduring
	loopScoped
	Header
	Process tool.ProcessLifecycleMetadata `json:"process"`
}

// ProcessBackgrounded durably records the transition from a foreground tool
// call to a session-owned background process.
type ProcessBackgrounded struct {
	enduring
	loopScoped
	Header
	Process tool.ProcessLifecycleMetadata `json:"process"`
}

// ProcessCompleted durably records one terminal supervised-process outcome.
type ProcessCompleted struct {
	enduring
	loopScoped
	Header
	Process tool.ProcessLifecycleMetadata `json:"process"`
}

// ProcessStopRequested durably records a nonterminal portable stop request.
type ProcessStopRequested struct {
	enduring
	loopScoped
	Header
	Process tool.ProcessLifecycleMetadata `json:"process"`
}

// ProcessLost durably records a previously nonterminal local process that
// cannot be reattached after restore.
type ProcessLost struct {
	enduring
	loopScoped
	Header
	Process tool.ProcessLifecycleMetadata `json:"process"`
}

func (ProcessStarted) isEvent()       {}
func (ProcessBackgrounded) isEvent()  {}
func (ProcessCompleted) isEvent()     {}
func (ProcessStopRequested) isEvent() {}
func (ProcessLost) isEvent()          {}
