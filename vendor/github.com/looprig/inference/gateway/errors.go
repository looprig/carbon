package gateway

import (
	"fmt"

	"github.com/looprig/inference/model"
)

// ConfigError reports invalid gateway routing configuration supplied to
// NewMux or Fixed. It covers every "invalid configuration" case this
// package rejects at construction time:
//
//   - a nil Client on any Target (in Routes, FormatDefaults, or Default);
//   - a Target.Model that fails model.Model.Validate (Err wraps the
//     underlying *model.ValidationError, reachable via errors.As/errors.Is);
//   - a duplicate logical route: the same non-empty Target.ID bound, across
//     Routes/FormatDefaults/Default, to two Targets whose Model values are
//     not equal (see NewMux's doc comment for the full rule).
//
// Location identifies where in the configuration the problem was found
// (e.g. "Routes[anthropic/primary]", "FormatDefaults[openai]", "Default"),
// for diagnosis; it is not part of any equality contract.
type ConfigError struct {
	Location string
	Reason   string
	Err      error
}

func (e *ConfigError) Error() string {
	msg := fmt.Sprintf("gateway: invalid configuration at %s: %s", e.Location, e.Reason)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap exposes a wrapped validation cause (e.g. *model.ValidationError) to
// errors.Is/errors.As. It is nil when Reason alone is self-explanatory.
func (e *ConfigError) Unwrap() error { return e.Err }

// RouteNotFoundError reports a Resolve miss: no exact route matched Ingress
// and Model, no FormatDefaults entry matched Ingress, and no global Default
// was configured.
type RouteNotFoundError struct {
	Ingress model.APIFormat
	Model   string
}

func (e *RouteNotFoundError) Error() string {
	return fmt.Sprintf("gateway: no route for ingress format %q model %q", e.Ingress, e.Model)
}
