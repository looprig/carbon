// Package retry decorates an inference.Client with bounded, classified
// retry and exponential backoff. It retries Invoke calls and Stream
// establishment only; once a StreamReader is handed out, a mid-stream
// failure is terminal for the wrapper exactly as for the inner client.
package retry

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/stream"
)

// Policy is the immutable retry schedule: StableRetries retries at
// StableDelay, then exponential doubling (starting at 2*StableDelay)
// capped at MaxDelay, until MaxAttempts total attempts have been made.
type Policy struct {
	StableRetries int           // retries at fixed StableDelay
	StableDelay   time.Duration // delay for the stable retry leg
	MaxAttempts   int           // total attempts including the first
	MaxDelay      time.Duration // cap on the exponential retry leg
}

// Validate reports the first structural defect. The zero value is invalid:
// this package never invents a schedule the caller did not state.
func (p Policy) Validate() error {
	switch {
	case p.MaxAttempts < 1:
		return &ConfigError{Field: "MaxAttempts", Reason: "must be at least 1"}
	case p.StableRetries < 0:
		return &ConfigError{Field: "StableRetries", Reason: "must not be negative"}
	case p.StableRetries >= p.MaxAttempts:
		return &ConfigError{Field: "StableRetries", Reason: "must be less than MaxAttempts (attempt 1 is not a retry)"}
	case p.StableDelay <= 0:
		return &ConfigError{Field: "StableDelay", Reason: "must be positive"}
	case p.MaxDelay < p.StableDelay:
		return &ConfigError{Field: "MaxDelay", Reason: "must be at least StableDelay"}
	}
	return nil
}

// ConfigError reports an invalid retry configuration at construction.
type ConfigError struct {
	Field  string
	Reason string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("retry: invalid config: %s %s", e.Field, e.Reason)
}

// Client decorates an inner inference.Client with the Policy's schedule.
type Client struct {
	inner  inference.Client
	policy Policy

	// Test seams; production values set by New.
	after     func(time.Duration) <-chan time.Time
	randFloat func() float64
}

var _ inference.Client = (*Client)(nil)

// New validates policy and returns the decorated client.
func New(inner inference.Client, policy Policy) (*Client, error) {
	if inner == nil {
		return nil, &ConfigError{Field: "inner", Reason: "client must not be nil"}
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &Client{inner: inner, policy: policy, after: time.After, randFloat: rand.Float64}, nil
}

func (c *Client) Invoke(ctx context.Context, req inference.Request) (*inference.Response, error) {
	resp, attempts, err := attemptLoop[*inference.Response](ctx, c, func() (*inference.Response, error) {
		return c.inner.Invoke(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, &InvalidResponseError{}
	}
	resp.Attempts = attempts
	return resp, nil
}

func (c *Client) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	inner, attempts, err := attemptLoop[*stream.StreamReader[content.Chunk]](ctx, c, func() (*stream.StreamReader[content.Chunk], error) {
		reader, err := c.inner.Stream(ctx, req)
		if err != nil && reader != nil {
			_ = reader.Close()
		}
		return reader, err
	})
	if err != nil {
		return nil, err
	}
	// Rewrap only to stamp Attempts into the terminal result; Next/Close
	// delegate, so mid-stream semantics are byte-identical to the inner reader.
	return stream.NewStreamReaderWithResult(inner.Next, inner.Close, func() (stream.StreamResult, bool, error) {
		result, ok := inner.Result()
		if !ok {
			return stream.StreamResult{}, false, nil
		}
		result.Attempts = attempts
		return result, true, nil
	}), nil
}

// attemptLoop drives the shared retry ladder for both entry points. T is the
// per-attempt success value (*inference.Response, or the stream reader).
func attemptLoop[T any](ctx context.Context, c *Client, call func() (T, error)) (T, int, error) {
	var zero T
	var lastErr error
	for attempt := 1; ; attempt++ {
		value, err := call()
		if err == nil {
			return value, attempt, nil
		}
		lastErr = err
		if !Retryable(err) {
			return zero, attempt, err
		}
		if attempt >= c.policy.MaxAttempts {
			return zero, attempt, &ExhaustedError{Attempts: attempt, Cause: lastErr}
		}
		select {
		case <-c.after(nextDelay(c.policy, attempt, err, c.randFloat())):
		case <-ctx.Done():
			return zero, attempt, ctx.Err()
		}
	}
}
