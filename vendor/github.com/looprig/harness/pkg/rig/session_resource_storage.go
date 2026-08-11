package rig

import (
	"context"
	"reflect"

	"github.com/looprig/core/uuid"
)

// SessionResourceStorage identifies the durable storage assigned to one
// session's shared resources. Path and Identity are opaque to Rig definition;
// the session lifecycle validates and consumes them when it resolves storage.
type SessionResourceStorage struct {
	Path     string
	Identity string
}

// SessionResourceStorageProvider resolves durable storage for a session.
//
// A provider is retained by the immutable Rig and may be called concurrently
// for multiple sessions, so implementations must be safe for concurrent use.
// Repeated calls for the same non-zero session ID, including after process
// restart, must resolve the same durable storage identity. The provider retains
// ownership of its own state; the harness receives only the returned value and
// does not mutate the provider.
type SessionResourceStorageProvider interface {
	StorageForSession(context.Context, uuid.UUID) (SessionResourceStorage, error)
}

// WithSessionResourceStorage installs the singleton durable-storage provider
// used by session-owned resources.
func WithSessionResourceStorage(provider SessionResourceStorageProvider) Option {
	return func(state *definitionState) error {
		if state.seen[keySessionResourceStorage] {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: string(keySessionResourceStorage)}
		}
		if nilSessionResourceStorageProvider(provider) {
			return &DefinitionError{Kind: DefinitionInvalidResourceStorage}
		}
		state.seen[keySessionResourceStorage] = true
		state.resourceStorageProvider = provider
		return nil
	}
}

func nilSessionResourceStorageProvider(provider SessionResourceStorageProvider) bool {
	if provider == nil {
		return true
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
