// Package contextcount provides deterministic complete-request context counting.
package contextcount

import (
	"fmt"

	model "github.com/looprig/inference/model"
)

// EstimatorStateReason identifies why an estimator cannot produce a count.
type EstimatorStateReason string

const (
	EstimatorStateNilReceiver EstimatorStateReason = "nil receiver"
	EstimatorStateNilContext  EstimatorStateReason = "nil context"
)

// EstimatorStateError reports invalid estimator state.
type EstimatorStateError struct {
	Reason EstimatorStateReason
}

func (e *EstimatorStateError) Error() string {
	return "contextcount: invalid estimator state: " + string(e.Reason)
}

// ModelIdentityError reports an unresolved request model identity.
type ModelIdentityError struct {
	Model model.ModelKey
	Err   error
}

func (e *ModelIdentityError) Error() string {
	return fmt.Sprintf("contextcount: invalid model identity %q/%q: %v", e.Model.Provider, e.Model.Model, e.Err)
}

func (e *ModelIdentityError) Unwrap() error { return e.Err }

// UnsupportedAPIFormatError reports a request dialect without a bundled encoder.
type UnsupportedAPIFormatError struct {
	APIFormat model.APIFormat
}

func (e *UnsupportedAPIFormatError) Error() string {
	return fmt.Sprintf("contextcount: unsupported API format %q", e.APIFormat)
}

// RequestEncodingError reports a dialect encoder failure and preserves its cause.
type RequestEncodingError struct {
	APIFormat model.APIFormat
	Err       error
}

func (e *RequestEncodingError) Error() string {
	return fmt.Sprintf("contextcount: encode %q request: %v", e.APIFormat, e.Err)
}

func (e *RequestEncodingError) Unwrap() error { return e.Err }
