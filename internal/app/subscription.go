package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/looprig/credentials"
	"github.com/looprig/credentials/refresh"
	"github.com/looprig/inference/model"
	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/openaisubscription"
	"github.com/looprig/secrets"
)

var ErrCredentialLoginActive = errors.New("carbon: credential sign-in requires idle sessions")

type subscriptionLoginFunc func(context.Context, openaisubscription.LoginOptions) (refresh.State, openaisubscription.Registration, error)

// The host-visible coordinator is opened only for OAuth. Platforms that do
// not support it retain Carbon's existing API-key credential behavior.
func (r *credentialRuntime) ensureSubscriptionCoordinatorLocked() error {
	if r.coordinator != nil {
		return nil
	}
	coordinator, err := refresh.NewFileCoordinator(filepath.Join(r.home, "credentials", "catalog"))
	if err != nil {
		return &CredentialCompositionError{Reason: "open subscription refresh coordinator"}
	}
	r.coordinator = coordinator
	return nil
}

func subscriptionHostID(ctx context.Context, r *credentialRuntime) (string, error) {
	// Create-only persistence makes concurrent first launches adopt one host ID.
	ref, _ := secrets.NewReference("local", "identity/openai-subscription-host")
	record, err := r.store.Resolve(ctx, ref)
	if err == nil {
		raw := record.Value.Bytes()
		defer clear(raw)
		return string(raw), nil
	}
	if !errors.Is(err, secrets.ErrNotFound) {
		return "", &CredentialCompositionError{Reason: "read subscription host identity"}
	}
	host, err := openaisubscription.NewHostID()
	if err != nil {
		return "", err
	}
	value, err := secrets.New([]byte(host))
	if err != nil {
		return "", err
	}
	if _, err := r.store.Put(ctx, ref, value, secrets.CreateOnlyPut()); err != nil {
		if errors.Is(err, secrets.ErrConflict) {
			return subscriptionHostID(ctx, r)
		}
		return "", &CredentialCompositionError{Reason: "persist subscription host identity"}
	}
	return host, nil
}

func subscriptionRegistrationRef(name string) secrets.Reference {
	ref, _ := secrets.NewReference("local", "identity/openai-subscription/"+name)
	return ref
}
func savedSubscriptionRegistration(ctx context.Context, r *credentialRuntime, ref credentials.Reference) (openaisubscription.Registration, error) {
	mapping, err := r.store.Resolve(ctx, subscriptionRegistrationRef(ref.Name()))
	if err != nil {
		return openaisubscription.Registration{}, &CredentialCompositionError{Reference: ref, Reason: "read saved subscription registration"}
	}
	raw := mapping.Value.Bytes()
	defer clear(raw)
	var registration openaisubscription.Registration
	if json.Unmarshal(raw, &registration) != nil || registration.Validate() != nil || registration.AccountName() != ref.Name() {
		return openaisubscription.Registration{}, openaisubscription.ErrIdentity
	}
	// A retained verified ID token is a login hint only while locally signed in.
	if record, err := r.catalog.Get(ctx, ref); err == nil {
		stateRecord, err := r.store.Resolve(ctx, record.State)
		if err != nil {
			return openaisubscription.Registration{}, &CredentialCompositionError{Reason: "read subscription state"}
		}
		state, err := refresh.DecodeState(stateRecord.Value)
		if err != nil {
			return openaisubscription.Registration{}, openaisubscription.ErrIdentity
		}
		current, err := openaisubscription.RegistrationFromState(state)
		if err != nil || current.ClientID != registration.ClientID || current.Subject != registration.Subject {
			return openaisubscription.Registration{}, openaisubscription.ErrIdentity
		}
		registration.IDToken = current.IDToken
	} else if !errors.Is(err, credentials.ErrCatalogNotFound) {
		return openaisubscription.Registration{}, &CredentialCompositionError{Reason: "read subscription catalog"}
	}
	return registration, nil
}

func loginSubscription(ctx context.Context, r *credentialRuntime, rawRef string, login subscriptionLoginFunc) error {
	if ctx == nil {
		return credentials.ErrNilContext
	}
	r.mu.Lock()
	if r.closed || r.closing {
		r.mu.Unlock()
		return ErrCredentialLifecycleClosed
	}
	if r.activeN > 0 {
		r.mu.Unlock()
		return ErrCredentialLoginActive
	}
	if err := r.ensureSubscriptionCoordinatorLocked(); err != nil {
		r.mu.Unlock()
		return err
	}
	if r.operations == 0 {
		r.opDone = make(chan struct{})
	}
	r.operations++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.operations--
		if r.operations == 0 {
			close(r.opDone)
		}
		r.mu.Unlock()
	}()
	var previous *openaisubscription.Registration
	if rawRef != "" {
		ref, err := credentials.ParseReference(rawRef)
		if err != nil || ref.Provider() != "openai-subscription" {
			return &CredentialCompositionError{Reason: "invalid subscription credential reference"}
		}
		registration, err := savedSubscriptionRegistration(ctx, r, ref)
		if err != nil {
			return err
		}
		previous = &registration
	}
	host, err := subscriptionHostID(ctx, r)
	if err != nil {
		return err
	}
	state, registration, err := login(ctx, openaisubscription.LoginOptions{HostID: host, Previous: previous, HTTPClient: r.httpClient, OpenBrowser: func(url string) error { return openSubscriptionBrowser(ctx, url) }})
	if err != nil {
		return err
	}
	if registration.Validate() != nil || registration.HostID != host || previous != nil && (registration.Subject != previous.Subject || registration.ClientID != previous.ClientID) {
		return openaisubscription.ErrIdentity
	}
	saved, err := openaisubscription.RegistrationFromState(state)
	if err != nil || saved.ClientID != registration.ClientID || saved.Subject != registration.Subject || saved.HostID != host {
		return openaisubscription.ErrIdentity
	}
	ref, err := credentials.ParseReference("credential://openai-subscription/" + registration.AccountName())
	if err != nil {
		return err
	}
	value, err := refresh.EncodeState(state)
	if err != nil {
		return err
	}
	selected := model.CustomModel("openai-subscription", model.APIFormatOpenAIResponses, "", "subscription")
	policy, err := llm.AuthPolicyForModel(selected)
	if err != nil {
		return err
	}
	descriptor, err := policy.Accepted[0].Descriptor()
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.closing {
		return ErrCredentialLifecycleClosed
	}
	if r.activeN > 0 {
		return ErrCredentialLoginActive
	}
	if r.blocked[ref] {
		return ErrCredentialLogoutBlocked
	}
	// Mapping retains client/account identity after logout, but no token hints.
	mapping := registration
	mapping.IDToken = ""
	mappingRaw, err := json.Marshal(mapping)
	if err != nil {
		return openaisubscription.ErrIdentity
	}
	defer clear(mappingRaw)
	mappingValue, err := secrets.New(mappingRaw)
	if err != nil {
		return err
	}
	if _, err := r.store.Put(ctx, subscriptionRegistrationRef(ref.Name()), mappingValue, secrets.UnconditionalPut()); err != nil {
		return &CredentialCompositionError{Reason: "persist subscription registration"}
	}
	if source, ok := r.sources[ref].(*refresh.Source); ok {
		// Existing clients keep the same Source; atomically rotate its authority.
		return source.Reauthenticate(ctx, state)
	}
	return r.coordinator.WithLock(ctx, ref, func(ctx context.Context) error {
		record, err := r.catalog.Get(ctx, ref)
		if err == nil {
			if !credentialDescriptorsMatchPolicy(record.Descriptor, descriptor) {
				return &CredentialCompositionError{Reason: "subscription descriptor mismatch"}
			}
			current, err := r.store.Resolve(ctx, record.State)
			if err != nil {
				return &CredentialCompositionError{Reason: "read current subscription state"}
			}
			if _, err := r.store.Put(ctx, record.State, value, secrets.CompareAndSwapPut(current.Version)); err != nil {
				return &CredentialCompositionError{Reason: "rotate subscription state"}
			}
			return nil
		}
		if !errors.Is(err, credentials.ErrCatalogNotFound) {
			return &CredentialCompositionError{Reason: "read subscription catalog"}
		}
		stateRef, _ := secrets.NewReference("local", "credentials/openai-subscription/"+ref.Name())
		now := time.Now().UTC()
		record, err = credentials.NewRecord(ref, descriptor, stateRef, now, now)
		if err != nil {
			return err
		}
		return (credentials.StatePublisher{Catalog: r.catalog, Store: r.store, Namespace: r.namespace}).Create(ctx, record, value)
	})
}

func openSubscriptionBrowser(ctx context.Context, browserURL string) error {
	// Only the provider's pinned authorization endpoint can reach the OS opener.
	parsed, err := url.Parse(browserURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "auth.openai.com" || parsed.Path != "/api/accounts/authorize" || parsed.RawPath != "" || parsed.User != nil || parsed.Fragment != "" {
		return openaisubscription.ErrLogin
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(ctx, "open", browserURL) // #nosec G204 -- Pinned HTTPS URL is one argument; no shell or executable input.
	case "linux":
		command = exec.CommandContext(ctx, "xdg-open", browserURL) // #nosec G204 -- Pinned HTTPS URL is one argument; no shell or executable input.
	case "windows":
		command = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", browserURL) // #nosec G204 -- Pinned HTTPS URL is one argument; no shell or executable input.
	default:
		return openaisubscription.ErrLogin
	}
	if err := command.Run(); err != nil {
		return openaisubscription.ErrLogin
	}
	return nil
}

// ListCredentialModels explicitly fetches the selected subscription account's
// model list without opening a runtime session or rewriting models.json.
func ListCredentialModels(ctx context.Context, cfg Config, rawRef string) ([]openaisubscription.AvailableModel, error) {
	ref, err := credentials.ParseReference(rawRef)
	if err != nil || ref.Provider() != "openai-subscription" {
		return nil, &CredentialCompositionError{Reason: "models requires a subscription credential reference"}
	}
	home, err := looprigHome(cfg)
	if err != nil {
		return nil, err
	}
	lease, r, err := acquireCredentialRuntime(home)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	selected := model.CustomModel("openai-subscription", model.APIFormatOpenAIResponses, "", "catalog")
	source, err := r.sourceFor(ctx, selected, ref)
	if err != nil {
		return nil, err
	}
	return openaisubscription.ListModels(ctx, source, r.httpClient)
}

// logoutSubscription holds the host lock through revocation AND local deletion.
// A concurrent process cannot replace the revoked session before deletion.
func (r *credentialRuntime) logoutSubscription(ctx context.Context, record credentials.Record) (error, error) {
	r.mu.Lock()
	err := r.ensureSubscriptionCoordinatorLocked()
	r.mu.Unlock()
	if err != nil {
		return openaisubscription.ErrRevoke, err
	}
	revokeErr := error(openaisubscription.ErrRevoke)
	deleteErr := r.coordinator.WithLock(ctx, record.Reference, func(ctx context.Context) error {
		current, err := r.store.Resolve(ctx, record.State)
		if err != nil {
			revokeErr = openaisubscription.ErrRevoke
		} else {
			state, err := refresh.DecodeState(current.Value)
			if err != nil {
				revokeErr = openaisubscription.ErrRevoke
			} else {
				revokeErr = openaisubscription.Revoke(ctx, r.httpClient, state)
			}
		}
		return (credentials.StatePublisher{Catalog: r.catalog, Store: r.store, Namespace: r.namespace}).Delete(ctx, record)
	})
	return revokeErr, deleteErr
}
