package main

import (
	"strings"
	"testing"
)

func TestSubscriptionModelCommand(t *testing.T) {
	ref := "credential://openai-subscription/account-example"
	flags, err := parseFlags([]string{"credentials", "models", ref})
	if err != nil || flags.credentialModels != ref {
		t.Fatalf("models command: %+v %v", flags, err)
	}
	for _, args := range [][]string{{"credentials", "models"}, {"--credential-models", ""}, {"--credential-models", ref, "--login", "openai-subscription"}, {"--credential-models", ref, "--serve"}, {"--credential-models", ref, "--resume", "d4e48e7e-c0a6-43c2-94ba-02a40dcb4498"}} {
		if _, err := parseFlags(args); err == nil {
			t.Fatalf("accepted conflicting args: %v", args)
		}
	}
}

func TestCredentialCommandUsageNamesModels(t *testing.T) {
	_, reason := normalizeSubcommandArgs([]string{"credentials"})
	if !strings.Contains(reason, "models") {
		t.Fatalf("usage %q omits the models command", reason)
	}
}
