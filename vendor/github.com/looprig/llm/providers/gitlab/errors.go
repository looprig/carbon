package gitlab

import (
	"fmt"

	model "github.com/looprig/inference/model"
	"github.com/looprig/llm"
)

type CounterSupportError = llm.CounterSupportError

type ModelMappingError struct {
	Alias  string
	Format model.APIFormat
	Reason string
}

func (e *ModelMappingError) Error() string {
	return "gitlab: model mapping for " + e.Alias + " (" + string(e.Format) + "): " + e.Reason
}

type DirectAccessError struct {
	Status int
	Reason string
	Err    error
}

func (e *DirectAccessError) Error() string {
	if e.Status != 0 {
		return "gitlab: direct access: " + e.Reason + " (status " + fmt.Sprint(e.Status) + ")"
	}
	return "gitlab: direct access: " + e.Reason
}

func (e *DirectAccessError) Unwrap() error { return e.Err }
