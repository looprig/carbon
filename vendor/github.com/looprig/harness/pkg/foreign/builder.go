// Package foreign defines the composition seams for foreign loop backends.
package foreign

import (
	"context"
	"errors"
	"sync"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
)

// EventPublisher is the foreign loop's narrow consumer of the session event
// fan-in. A session satisfies it via PublishEvent.
type EventPublisher interface {
	PublishEvent(context.Context, event.Event) error
	PublishEventChecked(context.Context, event.Event) error
}

// Builder is the composition-root seam a session uses to construct a foreign loop.
// It returns the Backend and the minted ForeignSID, which the caller records.
type Builder func(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	pub EventPublisher,
	cfg loop.BoundDefinition,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
) (loop.Backend, string, error)

// ServicesBuilder is the additive foreign-loop construction seam. Services
// is passed last so existing builder argument order remains source-compatible.
type ServicesBuilder func(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	pub EventPublisher,
	cfg loop.BoundDefinition,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
	services Services,
) (loop.Backend, string, error)

var (
	errBuilderRegistryNil      = errors.New("foreign: builder registry unavailable")
	errEmptyBuilderProfile     = errors.New("foreign: builder profile required")
	errDuplicateBuilderProfile = errors.New("foreign: builder profile already registered")
	errNilBuilder              = errors.New("foreign: builder callbacks required")
	errNilServicesBuilder      = errors.New("foreign: services builder callbacks required")
	errBuilderRegistryShape    = errors.New("foreign: builder registry entry has an invalid shape")
)

// UnknownProfileError reports a profile that is not registered. Its message is
// intentionally bounded and does not include the requested profile or any
// construction detail.
type UnknownProfileError struct{}

func (*UnknownProfileError) Error() string {
	return "foreign: unknown builder profile"
}

type builderPair struct {
	build           Builder
	restored        RestoredBuilder
	servicesBuild   ServicesBuilder
	servicesRestore ServicesRestoredBuilder
}

// BuilderRegistry routes foreign-loop construction by the stable runtime
// profile key. The zero value is ready for use. Registration is serialized and
// lookup takes a snapshot of the function pair, so a configured registry can
// be safely composed and read concurrently.
//
// A BuilderRegistry must not be copied after first use.
type BuilderRegistry struct {
	mu       sync.RWMutex
	builders map[loop.RuntimeProfileName]builderPair
}

// Register binds a live/restored builder pair to profile. Empty profiles and
// duplicate registrations fail closed; an existing binding is never replaced.
func (r *BuilderRegistry) Register(profile loop.RuntimeProfileName, builder Builder, restored RestoredBuilder) error {
	if r == nil {
		return errBuilderRegistryNil
	}
	if profile == "" {
		return errEmptyBuilderProfile
	}
	if builder == nil || restored == nil {
		return errNilBuilder
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.builders == nil {
		r.builders = make(map[loop.RuntimeProfileName]builderPair)
	}
	if _, exists := r.builders[profile]; exists {
		return errDuplicateBuilderProfile
	}
	r.builders[profile] = builderPair{build: builder, restored: restored}
	return nil
}

// RegisterServices binds a services-aware live/restored builder pair to a
// profile. A profile has one registration shape; use Register for legacy
// builders and RegisterServices for the additive services shape.
func (r *BuilderRegistry) RegisterServices(profile loop.RuntimeProfileName, builder ServicesBuilder, restored ServicesRestoredBuilder) error {
	if r == nil {
		return errBuilderRegistryNil
	}
	if profile == "" {
		return errEmptyBuilderProfile
	}
	if builder == nil || restored == nil {
		return errNilServicesBuilder
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.builders == nil {
		r.builders = make(map[loop.RuntimeProfileName]builderPair)
	}
	if _, exists := r.builders[profile]; exists {
		return errDuplicateBuilderProfile
	}
	r.builders[profile] = builderPair{servicesBuild: builder, servicesRestore: restored}
	return nil
}

// Builder returns legacy-shaped live and restored builders registered for
// profile. Services registrations are adapted with zero Services so legacy
// callers remain source-compatible without gaining authority. An unknown
// profile returns a bounded *UnknownProfileError and no builders.
func (r *BuilderRegistry) Builder(profile loop.RuntimeProfileName) (Builder, RestoredBuilder, error) {
	if r == nil {
		return nil, nil, &UnknownProfileError{}
	}

	r.mu.RLock()
	pair, exists := r.builders[profile]
	r.mu.RUnlock()
	if !exists {
		return nil, nil, &UnknownProfileError{}
	}
	switch {
	case pair.build != nil && pair.restored != nil && pair.servicesBuild == nil && pair.servicesRestore == nil:
		return pair.build, pair.restored, nil
	case pair.build == nil && pair.restored == nil && pair.servicesBuild != nil && pair.servicesRestore != nil:
		return adaptServicesBuilder(pair.servicesBuild), adaptServicesRestoredBuilder(pair.servicesRestore), nil
	default:
		return nil, nil, errBuilderRegistryShape
	}
}

// ServicesBuilder returns the services-aware live/restored builders for
// profile. Legacy registrations are adapted with a zero Services value so
// callers can use one dispatch path without changing legacy behavior.
func (r *BuilderRegistry) ServicesBuilder(profile loop.RuntimeProfileName) (ServicesBuilder, ServicesRestoredBuilder, error) {
	if r == nil {
		return nil, nil, &UnknownProfileError{}
	}

	r.mu.RLock()
	pair, exists := r.builders[profile]
	r.mu.RUnlock()
	if !exists {
		return nil, nil, &UnknownProfileError{}
	}
	switch {
	case pair.servicesBuild != nil && pair.servicesRestore != nil && pair.build == nil && pair.restored == nil:
		return pair.servicesBuild, pair.servicesRestore, nil
	case pair.servicesBuild == nil && pair.servicesRestore == nil && pair.build != nil && pair.restored != nil:
		return adaptBuilder(pair.build), adaptRestoredBuilder(pair.restored), nil
	default:
		return nil, nil, errBuilderRegistryShape
	}
}

// HasServicesBuilder reports whether profile was registered through the
// additive services-aware shape. Legacy registrations are intentionally false
// even though ServicesBuilder can return a compatibility adapter for them;
// callers that manage capabilities must not mint authority for a legacy
// builder that cannot receive it.
func (r *BuilderRegistry) HasServicesBuilder(profile loop.RuntimeProfileName) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	pair, exists := r.builders[profile]
	r.mu.RUnlock()
	return exists && pair.servicesBuild != nil && pair.servicesRestore != nil && pair.build == nil && pair.restored == nil
}
