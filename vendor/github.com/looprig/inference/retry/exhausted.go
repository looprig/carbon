package retry

import "fmt"

// ExhaustedError reports that every permitted attempt failed. Unwrap exposes
// the final attempt's failure so typed inspection (errors.As on
// *failure.APIError / *failure.NetworkError) keeps working.
type ExhaustedError struct {
	Attempts int
	Cause    error
}

func (e *ExhaustedError) Error() string {
	return fmt.Sprintf("retry: %d attempts exhausted: %v", e.Attempts, e.Cause)
}

func (e *ExhaustedError) Unwrap() error { return e.Cause }
