package model

import "fmt"

// ValidationError is a structurally invalid model or sampling value.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("inference: validation error: %s: %s", e.Field, e.Reason)
}
