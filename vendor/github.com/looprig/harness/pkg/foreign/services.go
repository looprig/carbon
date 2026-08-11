package foreign

import (
	"context"
	"fmt"
	"io"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
)

// BrokerDescriptor is the opaque, per-loop description of a collaboration
// broker. Harness intentionally keeps the endpoint and capability together;
// executable discovery, protocol selection, and transport construction belong
// to the composition root that consumes this value.
//
// The capability is never returned by reference. BrokerDescriptor has no
// String method so a secret cannot acquire a public formatting contract.
type BrokerDescriptor struct {
	endpoint   string
	capability []byte
}

const (
	redactedBrokerDescriptor = "<broker-descriptor-redacted>"
	redactedForeignServices  = "<foreign-services-redacted>"
)

// Format deliberately ignores the requested verb, flags, width, and
// precision. The descriptor may carry a bearer capability and an endpoint
// path, so every formatting form receives the same fixed bounded redaction.
func (d BrokerDescriptor) Format(state fmt.State, verb rune) {
	_, _ = io.WriteString(state, redactedBrokerDescriptor)
}

// NewBrokerDescriptor takes an endpoint and capability snapshot. The input
// capability is copied before the descriptor is returned.
func NewBrokerDescriptor(endpoint string, capability []byte) BrokerDescriptor {
	return BrokerDescriptor{endpoint: endpoint, capability: cloneBytes(capability)}
}

// Endpoint returns the broker endpoint from the immutable descriptor.
func (d BrokerDescriptor) Endpoint() string { return d.endpoint }

// Capability returns an independent copy of the opaque broker capability.
func (d BrokerDescriptor) Capability() []byte { return cloneBytes(d.capability) }

func (d BrokerDescriptor) clone() BrokerDescriptor {
	d.capability = cloneBytes(d.capability)
	return d
}

// DeliveryIntent identifies one durable delivery request. It deliberately
// carries only loop/request identity; the session binds the exact command
// payload privately before actor admission, so session controllers, journals,
// and message payloads do not cross the foreign-loop boundary.
type DeliveryIntent struct {
	LoopID    uuid.UUID
	RequestID uuid.UUID
}

// DeliveryReservation identifies one reserved foreign delivery attempt.
type DeliveryReservation = DeliveryIntent

// DeliveryFallback identifies the one normal-queue fallback for a request. The
// hook implementation reuses the command already bound to RequestID and writes
// its fallback phase before returning; callers never supply a second payload.
type DeliveryFallback = DeliveryIntent

// DeliveryResolutionState is the provider-neutral terminal classification for
// one foreign delivery attempt. A successful injected fold carries its turn
// identity; unknown and untrackable outcomes do not.
type DeliveryResolutionState string

const (
	DeliveryResolutionInjected    DeliveryResolutionState = "injected"
	DeliveryResolutionUnknown     DeliveryResolutionState = "unknown"
	DeliveryResolutionUntrackable DeliveryResolutionState = "untrackable"
)

// DeliveryResolution identifies a durable delivery resolution. TurnID is
// optional for an ambiguous or untrackable attempt and is present when the
// actor has a host-owned injected fold to correlate.
type DeliveryResolution struct {
	LoopID    uuid.UUID
	RequestID uuid.UUID
	TurnID    uuid.UUID
	State     DeliveryResolutionState
}

// DeliveryHook is the narrow durability capability supplied to one foreign
// loop actor. Implementations must scope every operation to the loop and
// request identifiers supplied in its value; they must not expose a Session,
// controller, journal, command sink, or other cross-loop authority. A
// successful QueueFallback return means its exact command payload is already
// durably recorded and may now be admitted through the normal actor path.
type DeliveryHook interface {
	CreateIntent(context.Context, DeliveryIntent) error
	Reserve(context.Context, DeliveryReservation) error
	QueueFallback(context.Context, DeliveryFallback) error
	Resolve(context.Context, DeliveryResolution) error
}

// Services is the immutable value supplied to a services-aware foreign
// builder. The zero value is the compatibility snapshot passed to legacy
// builders and carries no broker or delivery authority.
type Services struct {
	Broker   BrokerDescriptor
	Delivery DeliveryHook
}

// Format keeps the descriptor and delivery authority out of diagnostics even
// when Services is formatted as a struct with %#v or %+v. Formatting options
// are intentionally ignored for the same fixed bounded output contract as
// BrokerDescriptor.Format.
func (s Services) Format(state fmt.State, verb rune) {
	_, _ = io.WriteString(state, redactedForeignServices)
}

var (
	_ fmt.Formatter = BrokerDescriptor{}
	_ fmt.Formatter = Services{}
)

// NewServices takes an independent snapshot of broker descriptor bytes while
// retaining the narrow delivery interface value.
func NewServices(broker BrokerDescriptor, delivery DeliveryHook) Services {
	return Services{Broker: broker.clone(), Delivery: delivery}
}

// Clone returns an independent services snapshot. Interface values are copied
// as values; the hook implementation remains responsible for its own
// concurrency and loop scoping.
func (s Services) Clone() Services {
	s.Broker = s.Broker.clone()
	return s
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	copyOf := make([]byte, len(value))
	copy(copyOf, value)
	return copyOf
}

// adaptBuilder gives a legacy builder the additive services shape while
// preserving its historical zero-services behavior.
func adaptBuilder(builder Builder) ServicesBuilder {
	if builder == nil {
		return nil
	}
	return func(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance,
		pub EventPublisher, cfg loop.BoundDefinition, idGen func() (uuid.UUID, error),
		fac *event.Factory, _ Services) (loop.Backend, string, error) {
		return builder(loopCtx, sessionID, loopID, parent, pub, cfg, idGen, fac)
	}
}

// adaptRestoredBuilder gives a legacy restored builder the additive services
// shape while preserving its historical zero-services behavior.
func adaptRestoredBuilder(builder RestoredBuilder) ServicesRestoredBuilder {
	if builder == nil {
		return nil
	}
	return func(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance,
		pub EventPublisher, cfg loop.BoundDefinition, idGen func() (uuid.UUID, error),
		fac *event.Factory, seed RestoredForeign, _ Services) (loop.Backend, error) {
		return builder(loopCtx, sessionID, loopID, parent, pub, cfg, idGen, fac, seed)
	}
}

// adaptServicesBuilder gives a services-aware builder the legacy shape. The
// legacy caller has no authority to supply services, so the adapter always
// invokes it with the zero value rather than forwarding a shared capability.
func adaptServicesBuilder(builder ServicesBuilder) Builder {
	if builder == nil {
		return nil
	}
	return func(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance,
		pub EventPublisher, cfg loop.BoundDefinition, idGen func() (uuid.UUID, error),
		fac *event.Factory) (loop.Backend, string, error) {
		return builder(loopCtx, sessionID, loopID, parent, pub, cfg, idGen, fac, Services{})
	}
}

// adaptServicesRestoredBuilder gives a services-aware restored builder the
// legacy shape, also withholding all services authority from the old caller.
func adaptServicesRestoredBuilder(builder ServicesRestoredBuilder) RestoredBuilder {
	if builder == nil {
		return nil
	}
	return func(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance,
		pub EventPublisher, cfg loop.BoundDefinition, idGen func() (uuid.UUID, error),
		fac *event.Factory, seed RestoredForeign) (loop.Backend, error) {
		return builder(loopCtx, sessionID, loopID, parent, pub, cfg, idGen, fac, seed, Services{})
	}
}
