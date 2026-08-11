package task

import (
	"encoding/json"
	"sort"
)

// Status is the persisted state of a task.
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
)

func (status Status) valid() bool {
	switch status {
	case StatusPending, StatusInProgress, StatusCompleted:
		return true
	default:
		return false
	}
}

type taskRecord struct {
	ID          string
	Subject     string
	Description string
	ActiveForm  string
	Status      Status
	BlockedBy   []string
	Metadata    json.RawMessage
}

type Task struct {
	ID          string          `json:"id"`
	Subject     string          `json:"subject"`
	Description string          `json:"description"`
	ActiveForm  string          `json:"activeForm,omitempty"`
	Status      Status          `json:"status"`
	BlockedBy   []string        `json:"blockedBy,omitempty"`
	Blocks      []string        `json:"blocks,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
}

func cloneRecord(record taskRecord) taskRecord {
	record.BlockedBy = cloneStrings(record.BlockedBy)
	record.Metadata = cloneRawMessage(record.Metadata)
	return record
}

func taskFromRecord(record taskRecord, records map[string]taskRecord) Task {
	record = cloneRecord(record)
	task := Task{
		ID:          record.ID,
		Subject:     record.Subject,
		Description: record.Description,
		ActiveForm:  record.ActiveForm,
		Status:      record.Status,
		BlockedBy:   record.BlockedBy,
		Metadata:    record.Metadata,
	}

	for _, candidate := range records {
		if containsString(candidate.BlockedBy, record.ID) {
			task.Blocks = append(task.Blocks, candidate.ID)
		}
	}
	sort.Strings(task.Blocks)
	return task
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func cloneRawMessage(value json.RawMessage) json.RawMessage {
	if value == nil {
		return nil
	}
	cloned := make(json.RawMessage, len(value))
	copy(cloned, value)
	return cloned
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
