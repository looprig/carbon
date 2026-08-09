package app

import (
	"testing"
	"time"

	"github.com/looprig/inference/retry"
)

func TestDefaultRetryPolicy_Valid(t *testing.T) {
	t.Parallel()
	if err := defaultRetryPolicy.Validate(); err != nil {
		t.Fatalf("defaultRetryPolicy invalid: %v", err)
	}
	if defaultRetryPolicy.StableRetries != 3 || defaultRetryPolicy.StableDelay != 2*time.Second {
		t.Fatalf("agreed schedule drifted: %+v", defaultRetryPolicy)
	}
	if defaultRetryPolicy.MaxAttempts != 10 || defaultRetryPolicy.MaxDelay != 256*time.Second {
		t.Fatalf("production retry budget drifted: %+v", defaultRetryPolicy)
	}
}

func TestNewProductionClient_Wrapped(t *testing.T) {
	t.Parallel()
	c, err := newProductionClient(testModel(), modelClientInput{APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.(*retry.Client); !ok {
		t.Fatalf("production client not retry-wrapped: %T", c)
	}
}
