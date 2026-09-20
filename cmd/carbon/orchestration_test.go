package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestStockServeRefusesWithoutConfiguredVerifier(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"serve", "--data-dir", t.TempDir()}, &out, &errOut)
	if code != exitFailed || !strings.Contains(errOut.String(), ErrServeFactoryVerifierRequired.Error()) {
		t.Fatalf("stock serve = code %d stderr %q, want verifier-required refusal", code, errOut.String())
	}
}
