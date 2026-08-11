package model

// Effort is dialect-neutral "how hard to think" intent. Each codec maps it to its wire
// mechanism (openaiapi → reasoning_effort; anthropicapi → adaptive thinking + effort). Zero
// value (EffortNone) means the model decides / thinking off.
type Effort string

const (
	EffortNone   Effort = ""
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortMax    Effort = "max"
)

// Valid reports whether e is a known effort level (the empty value is valid = unset).
func (e Effort) Valid() bool {
	switch e {
	case EffortNone, EffortLow, EffortMedium, EffortHigh, EffortMax:
		return true
	default:
		return false
	}
}
