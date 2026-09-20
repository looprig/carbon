//go:build integration

package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/looprig/carbon/browser"
	carbon "github.com/looprig/carbon/internal/app"
	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/host"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
)

type serveTestVerifier struct{}

func (serveTestVerifier) VerifyCredential(_ context.Context, credential identity.Credential) (identity.Claims, error) {
	if credential.Value() != "test-browser-token" {
		return identity.Claims{}, identity.ErrUnauthenticated
	}
	return identity.Claims{Subject: "browser-user", Kind: identity.KindActor, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func TestInjectedBrowserLifecycleStartsAndStopsInOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errOut syncBuffer
	dataDir := t.TempDir()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	config := browserStartConfig{
		Storage: carbon.ServeStorageConfig{DataDir: dataDir, DefaultTenant: "local"},
		Host: carbon.ServePooledHostConfig{
			ListenAddress: "127.0.0.1:0", AuthToken: "test-host-token", StorageBindingID: "carbon-local-v1",
			Options: host.Options{HostID: "carbon-lifecycle", IsolationClass: sessionwire.HostIsolationClassCrossTenantIsolated,
				Placement: sessionwire.HostPlacementPooled, Capacity: 2, WarmTTL: time.Minute, RegistryHeartbeat: 10 * time.Second,
				RegistryExpiry: time.Minute, ClaimTTL: 5 * time.Second, ApplyDeadline: 30 * time.Second,
				CommandQueueSize: 16, ReconcileInterval: time.Minute, ReconcileBatch: 32},
			Generation: 1, Link: host.LinkOptions{MaxBindingsPerLink: 2, MaxBindings: 4, MaxTenantLinks: 2},
			Drain:                host.DrainOptions{Grace: 30 * time.Second, IdleBoundary: 10 * time.Second, PublishBound: 5 * time.Second},
			CompatibilityTimeout: 20 * time.Second, WorkPoll: time.Second,
		},
		Factory: browser.FactoryConfig{
			DefaultTenant: "local", StorageBindingID: "carbon-local-v1", BindingVersion: "v1",
			HostLinkToken: "test-host-token", ReplicaID: "carbon-lifecycle", CookieName: "carbon_session",
			Verifier: serveTestVerifier{}, Authorizer: factory.TenantAuthorizer{},
			CSRF: identity.CSRFConfig{SharedKey: bytes.Repeat([]byte{'k'}, identity.MinCSRFSharedKeyBytes), TokenTTL: time.Hour,
				TrustedOrigins: []string{"http://127.0.0.1"}},
		},
		Address: "127.0.0.1:0",
		ClientBuilder: func() (inference.Client, func() model.Model, error) {
			return &scriptedClient{fn: func(int, inference.Request) []content.Chunk { return nil }}, func() model.Model { return testServeModel() }, nil
		},
	}
	done := make(chan int, 1)
	go func() {
		done <- runWithBrowserConfig(ctx, []string{"serve", "--addr", "127.0.0.1:0", "--data-dir", dataDir,
			"--access-profile", "trusted"}, config, &out, &errOut)
	}()
	printed := waitForSubstring(t, &out, "http://127.0.0.1:")
	base := strings.TrimSpace(printed[strings.Index(printed, "http://"):])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/bootstrap", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-browser-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("browser bootstrap = %d", resp.StatusCode)
	}
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("browser lifecycle exit = %d stderr %q", code, errOut.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("browser lifecycle did not finish shutdown")
	}
}
