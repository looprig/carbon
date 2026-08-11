package vertex

import "github.com/looprig/llm"

type ConfigurationReason string

const (
	ProjectOrLocationMissing ConfigurationReason = "project or location is missing"
	ProjectOrLocationInvalid ConfigurationReason = "project or location is invalid"
	ModelMissing             ConfigurationReason = "model name is missing"
)

type ConfigurationError struct {
	Reason ConfigurationReason
}

func (e *ConfigurationError) Error() string {
	return "google-vertex: configuration: " + string(e.Reason)
}

type CounterSupportError = llm.CounterSupportError
