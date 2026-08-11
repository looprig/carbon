// Package backend implements the concrete Harness foreign-loop backend.
package backend

import (
	"reflect"

	"github.com/looprig/foreignloops/driver"
	"github.com/looprig/harness/pkg/foreign"
)

// SIDMode selects whether the foreign session ID is known when the loop is
// constructed or learned later from the agent's first initialization event.
type SIDMode uint8

const (
	SIDPrebound SIDMode = iota
	SIDLateBound
)

// Config is the composition-root wiring consumed by the generic backend.
// Provider process and environment settings belong to concrete driver configs.
type Config struct {
	Agent   driver.Agent
	Cwd     string
	Posture driver.PermissionPosture
	SIDMode SIDMode
}

func validateConfig(cfg Config) error {
	switch {
	case nilLike(cfg.Agent):
		return &ConfigError{Field: "Config.Agent", Reason: "required"}
	case cfg.Cwd == "":
		return &ConfigError{Field: "Config.Cwd", Reason: "required"}
	case cfg.Posture != driver.PostureDefault && cfg.Posture != driver.PostureAcceptEdits:
		return &ConfigError{Field: "Config.Posture", Reason: "unknown"}
	case cfg.SIDMode != SIDPrebound && cfg.SIDMode != SIDLateBound:
		return &ConfigError{Field: "Config.SIDMode", Reason: "unknown"}
	default:
		return nil
	}
}

// nilLike recognizes a nil interface and an interface containing a typed nil.
// IsNil is only valid for nilable kinds, so concrete value implementations are
// accepted without risking a reflection panic.
func nilLike(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return reflected.IsNil()
	default:
		return false
	}
}

// cloneServices snapshots the opaque capabilities at the actor boundary. The
// delivery hook remains a narrow interface value; the broker descriptor bytes
// are copied by foreign.Services.Clone.
func cloneServices(services foreign.Services) foreign.Services {
	return services.Clone()
}
