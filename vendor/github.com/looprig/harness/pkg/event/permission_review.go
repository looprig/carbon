package event

import (
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
)

// PermissionReviewStarted records that one classifier began reviewing an
// already-open permission gate. It carries only stable identity and revision
// metadata; permission subject data and classifier inputs are deliberately
// absent from the durable event.
type PermissionReviewStarted struct {
	enduring
	loopScoped
	Header
	GateID             gate.ID     `json:"gate_id,omitzero"`
	ToolExecutionID    uuid.UUID   `json:"tool_execution_id,omitzero"`
	Classifier         hustle.Name `json:"classifier,omitzero"`
	ClassifierRevision string      `json:"classifier_revision,omitzero"`
}

// PermissionReviewCompleted records a classifier's closed terminal audit
// status. It deliberately excludes prompt, evidence, model output, rationale,
// gate candidates, and grant material.
type PermissionReviewCompleted struct {
	enduring
	loopScoped
	Header
	GateID             gate.ID                   `json:"gate_id,omitzero"`
	ToolExecutionID    uuid.UUID                 `json:"tool_execution_id,omitzero"`
	Classifier         hustle.Name               `json:"classifier,omitzero"`
	ClassifierRevision string                    `json:"classifier_revision,omitzero"`
	Status             gate.ReviewStatus         `json:"status,omitzero"`
	Risk               gate.ReviewRisk           `json:"risk,omitzero"`
	Authorization      gate.ReviewAuthorization  `json:"authorization,omitzero"`
	Categories         []gate.ReviewRiskCategory `json:"categories,omitzero"`
	AutoApproved       bool                      `json:"auto_approved,omitzero"`
}

func (PermissionReviewStarted) isEvent()   {}
func (PermissionReviewCompleted) isEvent() {}
