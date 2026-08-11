package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/looprig/core/uuid"
)

const (
	// maxTaskArgsBytes is enforced by Task 4 PrepareCall before raw JSON decode.
	// The storage layer accepts typed input and does not estimate request bytes.
	maxTaskArgsBytes    = 64 << 10
	maxTasksPerLoop     = 256
	maxSubjectBytes     = 512
	maxDescriptionBytes = 16 << 10
	maxActiveFormBytes  = 512
	maxMetadataBytes    = 16 << 10
	maxDependencies     = 128
	maxStoreBytes       = 2 << 20
)

// StatusCommandDeleted is accepted only by update and is never persisted.
const StatusCommandDeleted Status = "deleted"

type idSource func() (uuid.UUID, error)

type createInput struct {
	Subject     string
	Description string
	ActiveForm  string
	BlockedBy   []string
	Metadata    json.RawMessage
}

type updateInput struct {
	TaskID          string
	Subject         *string
	Description     *string
	ActiveForm      *string
	Status          *Status
	AddBlockedBy    []string
	RemoveBlockedBy []string
	Metadata        *json.RawMessage
}

type store struct {
	mu    sync.Mutex
	newID idSource
	tasks map[string]taskRecord
}

func newStore(source idSource) *store {
	if source == nil {
		source = uuid.New
	}
	return &store{
		newID: source,
		tasks: make(map[string]taskRecord),
	}
}

func (s *store) create(input createInput) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.tasks) >= maxTasksPerLoop {
		return Task{}, errors.New("task store limit exceeded: too many tasks")
	}

	dependencies, err := s.normalizeDependencies(input.BlockedBy)
	if err != nil {
		return Task{}, err
	}
	if strings.TrimSpace(input.Subject) == "" {
		return Task{}, errors.New("task subject is required")
	}
	if len(input.Subject) > maxSubjectBytes {
		return Task{}, errors.New("task subject exceeds the byte limit")
	}
	if strings.TrimSpace(input.Description) == "" {
		return Task{}, errors.New("task description is required")
	}
	if len(input.Description) > maxDescriptionBytes {
		return Task{}, errors.New("task description exceeds the byte limit")
	}
	if len(input.ActiveForm) > maxActiveFormBytes {
		return Task{}, errors.New("task active form exceeds the byte limit")
	}
	if len(input.Metadata) > maxMetadataBytes {
		return Task{}, errors.New("task metadata exceeds the byte limit")
	}

	source := s.newID
	if source == nil {
		source = uuid.New
	}
	id, err := source()
	if err != nil {
		return Task{}, fmt.Errorf("generate task ID: %w", err)
	}
	if id.IsZero() {
		return Task{}, errors.New("task ID source returned zero UUID")
	}
	idString := id.String()
	if _, exists := s.tasks[idString]; exists {
		return Task{}, fmt.Errorf("task ID collision: %s", idString)
	}

	candidate := taskRecord{
		ID:          idString,
		Subject:     input.Subject,
		Description: input.Description,
		ActiveForm:  input.ActiveForm,
		Status:      StatusPending,
		BlockedBy:   dependencies,
		Metadata:    cloneRawMessage(input.Metadata),
	}
	if storeRecordBytes(s.tasks)+recordBytes(candidate) > maxStoreBytes {
		return Task{}, errors.New("task store limit exceeded: aggregate size")
	}

	s.tasks[idString] = candidate
	return taskFromRecord(candidate, s.tasks), nil
}

func (s *store) get(id string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	parsed, err := uuid.Parse(id)
	if err != nil {
		return Task{}, fmt.Errorf("invalid task ID: %w", err)
	}
	canonicalID := parsed.String()
	record, ok := s.tasks[canonicalID]
	if !ok {
		return Task{}, fmt.Errorf("unknown task ID: %s", canonicalID)
	}
	return taskFromRecord(record, s.tasks), nil
}

func (s *store) update(input updateInput) (Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := validateTaskGraph(s.tasks); err != nil {
		return Task{}, false, fmt.Errorf("invalid live task graph: %w", err)
	}

	parsed, err := uuid.Parse(input.TaskID)
	if err != nil {
		return Task{}, false, fmt.Errorf("invalid task ID: %w", err)
	}
	taskID := parsed.String()
	if _, ok := s.tasks[taskID]; !ok {
		return Task{}, false, fmt.Errorf("unknown task ID: %s", taskID)
	}
	if err := validateUpdateInput(input); err != nil {
		return Task{}, false, err
	}

	candidate := cloneRecords(s.tasks)
	record := candidate[taskID]
	if input.Subject != nil {
		record.Subject = *input.Subject
	}
	if input.Description != nil {
		record.Description = *input.Description
	}
	if input.ActiveForm != nil {
		record.ActiveForm = *input.ActiveForm
	}
	if input.Status != nil && *input.Status != StatusCommandDeleted {
		record.Status = *input.Status
	}
	if input.Metadata != nil {
		record.Metadata = normalizeUpdateMetadata(*input.Metadata)
	}

	addDependencies, err := normalizeDependenciesForGraph(input.AddBlockedBy, candidate, taskID)
	if err != nil {
		return Task{}, false, err
	}
	removeDependencies, err := normalizeDependenciesForGraph(input.RemoveBlockedBy, candidate, taskID)
	if err != nil {
		return Task{}, false, err
	}
	blockedBy := make(map[string]struct{}, len(record.BlockedBy)+len(addDependencies))
	for _, dependency := range record.BlockedBy {
		blockedBy[dependency] = struct{}{}
	}
	for _, dependency := range addDependencies {
		blockedBy[dependency] = struct{}{}
	}
	for _, dependency := range removeDependencies {
		delete(blockedBy, dependency)
	}
	record.BlockedBy = sortedDependencySet(blockedBy)
	candidate[taskID] = record

	deleted := input.Status != nil && *input.Status == StatusCommandDeleted
	if deleted {
		delete(candidate, taskID)
		for id, remaining := range candidate {
			remaining.BlockedBy = removeDependency(remaining.BlockedBy, taskID)
			candidate[id] = remaining
		}
	}

	if err := validateTaskGraph(candidate); err != nil {
		return Task{}, false, err
	}

	s.tasks = candidate
	if deleted {
		return Task{}, true, nil
	}
	return taskFromRecord(candidate[taskID], s.tasks), false, nil
}

func (s *store) list() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, 0, len(s.tasks))
	for id := range s.tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	listed := make([]Task, 0, len(ids))
	for _, id := range ids {
		listed = append(listed, taskFromRecord(s.tasks[id], s.tasks))
	}
	return listed
}

func (s *store) normalizeDependencies(dependencies []string) ([]string, error) {
	return normalizeDependenciesForGraph(dependencies, s.tasks, "")
}

func normalizeDependenciesForGraph(dependencies []string, records map[string]taskRecord, selfID string) ([]string, error) {
	if len(dependencies) > maxDependencies {
		return nil, errors.New("task has too many dependencies")
	}

	normalized := make([]string, 0, len(dependencies))
	seen := make(map[string]struct{}, len(dependencies))
	for _, dependency := range dependencies {
		parsed, err := uuid.Parse(dependency)
		if err != nil {
			return nil, fmt.Errorf("invalid task dependency %q: %w", dependency, err)
		}
		canonicalID := parsed.String()
		if canonicalID == selfID {
			return nil, fmt.Errorf("task cannot depend on itself: %s", canonicalID)
		}
		if _, ok := records[canonicalID]; !ok {
			return nil, fmt.Errorf("unknown task dependency: %s", canonicalID)
		}
		if _, duplicate := seen[canonicalID]; duplicate {
			continue
		}
		seen[canonicalID] = struct{}{}
		normalized = append(normalized, canonicalID)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func validateUpdateInput(input updateInput) error {
	if input.Subject != nil && len(*input.Subject) > maxSubjectBytes {
		return errors.New("task subject exceeds the byte limit")
	}
	if input.Description != nil && len(*input.Description) > maxDescriptionBytes {
		return errors.New("task description exceeds the byte limit")
	}
	if input.ActiveForm != nil && len(*input.ActiveForm) > maxActiveFormBytes {
		return errors.New("task active form exceeds the byte limit")
	}
	if input.Metadata != nil && len(*input.Metadata) > maxMetadataBytes {
		return errors.New("task metadata exceeds the byte limit")
	}
	if input.Status != nil && *input.Status != StatusCommandDeleted && !input.Status.valid() {
		return fmt.Errorf("unknown task status: %s", *input.Status)
	}
	return nil
}

func normalizeUpdateMetadata(metadata json.RawMessage) json.RawMessage {
	if bytes.Equal(bytes.TrimSpace(metadata), []byte("{}")) {
		return nil
	}
	return cloneRawMessage(metadata)
}

func cloneRecords(records map[string]taskRecord) map[string]taskRecord {
	cloned := make(map[string]taskRecord, len(records))
	for id, record := range records {
		cloned[id] = cloneRecord(record)
	}
	return cloned
}

func sortedDependencySet(dependencies map[string]struct{}) []string {
	if len(dependencies) == 0 {
		return nil
	}
	result := make([]string, 0, len(dependencies))
	for dependency := range dependencies {
		result = append(result, dependency)
	}
	sort.Strings(result)
	return result
}

func removeDependency(dependencies []string, target string) []string {
	filtered := make([]string, 0, len(dependencies))
	for _, dependency := range dependencies {
		if dependency != target {
			filtered = append(filtered, dependency)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func validateTaskGraph(records map[string]taskRecord) error {
	if len(records) > maxTasksPerLoop {
		return errors.New("task store limit exceeded: too many tasks")
	}

	for id, record := range records {
		parsed, err := uuid.Parse(id)
		if err != nil {
			return fmt.Errorf("invalid task ID: %w", err)
		}
		if parsed.String() != id || record.ID != id {
			return fmt.Errorf("task record ID is not canonical: %s", id)
		}
		if len(record.Subject) > maxSubjectBytes {
			return fmt.Errorf("task subject exceeds the byte limit: %s", id)
		}
		if len(record.Description) > maxDescriptionBytes {
			return fmt.Errorf("task description exceeds the byte limit: %s", id)
		}
		if len(record.ActiveForm) > maxActiveFormBytes {
			return fmt.Errorf("task active form exceeds the byte limit: %s", id)
		}
		if len(record.Metadata) > maxMetadataBytes {
			return fmt.Errorf("task metadata exceeds the byte limit: %s", id)
		}
		if !record.Status.valid() {
			return fmt.Errorf("unknown task status: %s", record.Status)
		}
		if len(record.BlockedBy) > maxDependencies {
			return fmt.Errorf("task has too many dependencies: %s", id)
		}

		seen := make(map[string]struct{}, len(record.BlockedBy))
		for _, dependency := range record.BlockedBy {
			parsedDependency, err := uuid.Parse(dependency)
			if err != nil {
				return fmt.Errorf("invalid task dependency %q: %w", dependency, err)
			}
			canonicalDependency := parsedDependency.String()
			if canonicalDependency != dependency {
				return fmt.Errorf("task dependency is not canonical: %s", dependency)
			}
			if dependency == id {
				return fmt.Errorf("task cannot depend on itself: %s", id)
			}
			if _, ok := records[dependency]; !ok {
				return fmt.Errorf("unknown task dependency: %s", dependency)
			}
			if _, duplicate := seen[dependency]; duplicate {
				return fmt.Errorf("duplicate task dependency: %s", dependency)
			}
			seen[dependency] = struct{}{}
		}

		if record.Status == StatusInProgress {
			for _, dependency := range record.BlockedBy {
				if records[dependency].Status != StatusCompleted {
					return fmt.Errorf("in_progress task %s has incomplete blocker: %s", id, dependency)
				}
			}
		}
	}

	if total := storeRecordBytes(records); total > maxStoreBytes {
		return errors.New("task store limit exceeded: aggregate size")
	}
	return validateNoDependencyCycles(records)
}

func validateNoDependencyCycles(records map[string]taskRecord) error {
	const (
		visiting uint8 = iota + 1
		visited
	)
	state := make(map[string]uint8, len(records))
	var visit func(string, int) error
	visit = func(id string, depth int) error {
		if depth > len(records) {
			return errors.New("task dependency graph traversal exceeded bound")
		}
		switch state[id] {
		case visiting:
			return fmt.Errorf("task dependency cycle detected at %s", id)
		case visited:
			return nil
		}
		state[id] = visiting
		for _, dependency := range records[id].BlockedBy {
			if err := visit(dependency, depth+1); err != nil {
				return err
			}
		}
		state[id] = visited
		return nil
	}

	for id := range records {
		if err := visit(id, 0); err != nil {
			return err
		}
	}
	return nil
}

func storeRecordBytes(records map[string]taskRecord) int {
	total := 0
	for _, record := range records {
		total += recordBytes(record)
	}
	return total
}

func recordBytes(record taskRecord) int {
	total := len(record.ID) + len(record.Subject) + len(record.Description) + len(record.ActiveForm) + len(record.Status) + len(record.Metadata)
	for _, dependency := range record.BlockedBy {
		total += len(dependency)
	}
	return total
}
