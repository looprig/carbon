// Package collab contains the deliberately small collaboration boundary used
// by the injected MessageAgent MCP process. The package owns the broker wire
// DTOs and framing, but it does not import Harness or ACP types.
package collab

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
)

const (
	// EndpointEnv and TokenEnv are the only environment entries accepted by
	// carbon-collab-mcp. They are intentionally constants rather than flags so
	// a token can never arrive through process arguments.
	EndpointEnv = "CODERIG_COLLAB_ENDPOINT"
	TokenEnv    = "CODERIG_COLLAB_TOKEN" // #nosec G101 -- environment variable name, not a credential

	// Aliases make the environment contract explicit at call sites without
	// creating alternate accepted names.
	EndpointEnvName   = EndpointEnv
	TokenEnvName      = TokenEnv
	BrokerEndpointEnv = EndpointEnv
	BrokerTokenEnv    = TokenEnv

	ToolName = "MessageAgent"

	CapabilityBytes    = 32
	MaxCapabilityBytes = CapabilityBytes

	// MaxMessageBytes is the existing MessageAgent message bound.
	MaxMessageBytes = 192 << 10
	// MaxArgumentBytes bounds the encoded tools/call argument object.
	MaxArgumentBytes = 256 << 10
	// MaxFrameBytes bounds one broker length-prefixed JSON frame. It is large
	// enough for a maximum argument/result plus the small DTO envelope.
	MaxFrameBytes = MaxArgumentBytes
	// MaxEndpointBytes prevents an unbounded path from entering a dial call.
	MaxEndpointBytes  = 4096
	MaxTimeoutSeconds = 24 * 60 * 60

	defaultConnectTimeout   = 5 * time.Second
	defaultAdmissionTimeout = 5 * time.Second
)

var (
	ErrInvalidRequest      = errors.New("invalid collaboration request")
	ErrInvalidArguments    = ErrInvalidRequest
	ErrInputLimit          = errors.New("collaboration input exceeds limit")
	ErrInvalidCapability   = errors.New("invalid collaboration capability")
	ErrInvalidConfig       = errors.New("invalid collaboration configuration")
	ErrFrameLimit          = errors.New("collaboration frame exceeds limit")
	ErrFrameTooLarge       = ErrFrameLimit
	ErrFrame               = errors.New("invalid collaboration frame")
	ErrAuthentication      = errors.New("collaboration authentication failed")
	ErrAuth                = ErrAuthentication
	ErrConnection          = errors.New("collaboration connection failed")
	ErrAdmission           = errors.New("collaboration admission failed")
	ErrResponse            = errors.New("collaboration response failed")
	ErrDeadline            = errors.New("collaboration deadline exceeded")
	ErrUnsupportedPlatform = errors.New("collaboration IPC unsupported on this platform")
)

// MessageAgentRequest is the complete model-visible MessageAgent argument
// object. No identity or correlation field belongs here; the broker derives
// those values from the authenticated capability and runtime context.
type MessageAgentRequest struct {
	AgentID         string `json:"agent_id"`
	Message         string `json:"message"`
	WaitForResponse bool   `json:"wait_for_response"`
	TimeoutSeconds  *int   `json:"timeout_seconds,omitempty"`
}

// PreparedMessageAgent is a descriptive alias for a validated request.
type PreparedMessageAgent = MessageAgentRequest

// DelegateResult is the public, correlation-free result envelope returned by
// the broker. Internal request IDs and controller details are intentionally not
// represented by this type.
type DelegateResult struct {
	AgentID        string `json:"agent_id"`
	Name           string `json:"name"`
	State          string `json:"state"`
	DeliveryStatus string `json:"delivery_status,omitempty"`
	ResponseStatus string `json:"response_status,omitempty"`
	Response       string `json:"response,omitempty"`
}

// ClientConfig configures one authenticated broker call. Capability is a raw
// 32-byte value; the process environment uses its lowercase hexadecimal
// representation. Token is retained as a descriptive input alias for callers
// that use token terminology; supplying both fields is rejected unless they
// are byte-for-byte identical.
type ClientConfig struct {
	Endpoint   string
	Capability []byte
	Token      []byte

	ConnectTimeout   time.Duration
	AdmissionTimeout time.Duration
	MaxFrameBytes    int
}

// Config is a descriptive alias for ClientConfig.
type Config = ClientConfig

// ConfigFromEnv reads the fixed endpoint and token environment entries. The
// supplied lookup function keeps startup unit-testable without mutating the
// process environment.
func ConfigFromEnv(lookup func(string) (string, bool)) (ClientConfig, error) {
	if lookup == nil {
		return ClientConfig{}, ErrInvalidConfig
	}
	endpoint, endpointOK := lookup(EndpointEnv)
	tokenText, tokenOK := lookup(TokenEnv)
	if !endpointOK || !tokenOK {
		return ClientConfig{}, ErrInvalidConfig
	}
	capability, err := DecodeCapabilityToken(tokenText)
	if err != nil {
		return ClientConfig{}, ErrInvalidConfig
	}
	cfg := ClientConfig{Endpoint: endpoint, Capability: capability}
	if _, err := cfg.normalized(); err != nil {
		return ClientConfig{}, ErrInvalidConfig
	}
	return cfg, nil
}

func (c ClientConfig) normalized() (ClientConfig, error) {
	if c.Endpoint == "" || !filepath.IsAbs(c.Endpoint) || len(c.Endpoint) > MaxEndpointBytes || !utf8.ValidString(c.Endpoint) || strings.IndexByte(c.Endpoint, 0) >= 0 {
		return ClientConfig{}, ErrInvalidConfig
	}
	capability := c.Capability
	if capability == nil {
		capability = c.Token
	} else if c.Token != nil && !bytes.Equal(c.Capability, c.Token) {
		return ClientConfig{}, ErrInvalidConfig
	}
	if len(capability) != CapabilityBytes {
		return ClientConfig{}, ErrInvalidCapability
	}
	c.Capability = append([]byte(nil), capability...)
	c.Token = nil
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = defaultConnectTimeout
	}
	if c.AdmissionTimeout == 0 {
		c.AdmissionTimeout = defaultAdmissionTimeout
	}
	if c.MaxFrameBytes == 0 {
		c.MaxFrameBytes = MaxFrameBytes
	}
	if c.ConnectTimeout < 0 || c.AdmissionTimeout < 0 || c.MaxFrameBytes < 1 || c.MaxFrameBytes > MaxFrameBytes {
		return ClientConfig{}, ErrInvalidConfig
	}
	return c, nil
}

// DecodeCapabilityToken decodes the fixed hexadecimal environment format.
func DecodeCapabilityToken(encoded string) ([]byte, error) {
	if len(encoded) != CapabilityBytes*2 {
		return nil, ErrInvalidCapability
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != CapabilityBytes {
		return nil, ErrInvalidCapability
	}
	return decoded, nil
}

// EncodeCapabilityToken returns the lowercase hexadecimal environment format.
func EncodeCapabilityToken(capability []byte) (string, error) {
	if len(capability) != CapabilityBytes {
		return "", ErrInvalidCapability
	}
	return hex.EncodeToString(capability), nil
}

// DecodeMessageAgent strictly validates one MessageAgent argument object.
// Validation happens before any client dial or frame write.
func DecodeMessageAgent(raw []byte) (MessageAgentRequest, error) {
	if len(raw) > MaxArgumentBytes {
		return MessageAgentRequest{}, ErrInputLimit
	}
	if !utf8.Valid(raw) {
		return MessageAgentRequest{}, ErrInvalidRequest
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return MessageAgentRequest{}, ErrInvalidRequest
	}

	type wireRequest struct {
		AgentID         *string `json:"agent_id"`
		Message         *string `json:"message"`
		WaitForResponse *bool   `json:"wait_for_response"`
		TimeoutSeconds  *int    `json:"timeout_seconds"`
	}
	var wire wireRequest
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return MessageAgentRequest{}, ErrInvalidRequest
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return MessageAgentRequest{}, ErrInvalidRequest
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil || fields == nil {
		return MessageAgentRequest{}, ErrInvalidRequest
	}
	if wire.AgentID == nil || wire.Message == nil || !hasField(fields, "agent_id") || !hasField(fields, "message") {
		return MessageAgentRequest{}, ErrInvalidRequest
	}
	if wire.WaitForResponse == nil && hasField(fields, "wait_for_response") {
		return MessageAgentRequest{}, ErrInvalidRequest
	}
	if wire.TimeoutSeconds == nil && hasField(fields, "timeout_seconds") {
		return MessageAgentRequest{}, ErrInvalidRequest
	}

	if len(*wire.AgentID) > 36 || !utf8.ValidString(*wire.AgentID) {
		return MessageAgentRequest{}, ErrInvalidRequest
	}
	parsedID, err := uuid.Parse(*wire.AgentID)
	if err != nil || parsedID.IsZero() {
		return MessageAgentRequest{}, ErrInvalidRequest
	}
	if !utf8.ValidString(*wire.Message) || strings.TrimSpace(*wire.Message) == "" {
		return MessageAgentRequest{}, ErrInvalidRequest
	}
	if len(*wire.Message) > MaxMessageBytes {
		return MessageAgentRequest{}, ErrInputLimit
	}
	if wire.TimeoutSeconds != nil && (*wire.TimeoutSeconds < 0 || *wire.TimeoutSeconds > MaxTimeoutSeconds) {
		return MessageAgentRequest{}, ErrInvalidRequest
	}
	wait := true
	if wire.WaitForResponse != nil {
		wait = *wire.WaitForResponse
	}
	return MessageAgentRequest{AgentID: parsedID.String(), Message: *wire.Message, WaitForResponse: wait, TimeoutSeconds: cloneInt(wire.TimeoutSeconds)}, nil
}

// ValidateMessageAgent is an explicit spelling for DecodeMessageAgent.
func ValidateMessageAgent(raw []byte) (MessageAgentRequest, error) {
	return DecodeMessageAgent(raw)
}

func hasField(fields map[string]json.RawMessage, name string) bool {
	_, ok := fields[name]
	return ok
}

func validateMessageAgentRequest(request MessageAgentRequest) error {
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > MaxArgumentBytes {
		return ErrInvalidRequest
	}
	_, err = DecodeMessageAgent(encoded)
	return err
}

// DecodeDelegateResult validates a broker result and strips unknown internal
// fields from the public projection.
func DecodeDelegateResult(raw []byte) (DelegateResult, error) {
	if len(raw) == 0 || len(raw) > MaxFrameBytes || !utf8.Valid(raw) {
		return DelegateResult{}, ErrResponse
	}
	var result DelegateResult
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return DelegateResult{}, ErrResponse
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return DelegateResult{}, ErrResponse
	}
	if result.AgentID == "" || result.Name == "" || result.State == "" {
		return DelegateResult{}, ErrResponse
	}
	return result, nil
}

// WriteHandshake writes a length-prefixed raw binary capability. The length is
// bounded and the capability is never represented as JSON or text on the wire.
func WriteHandshake(w io.Writer, capability []byte) error {
	if len(capability) != CapabilityBytes {
		return ErrInvalidCapability
	}
	return writeFrameLimit(w, capability, CapabilityBytes)
}

// ReadHandshake reads and validates one length-prefixed raw capability.
func ReadHandshake(r io.Reader) ([]byte, error) {
	capability, err := readFrameLimit(r, CapabilityBytes)
	if err != nil {
		return nil, err
	}
	if len(capability) != CapabilityBytes {
		return nil, ErrInvalidCapability
	}
	return capability, nil
}

// WriteFrame writes one length-prefixed broker payload.
func WriteFrame(w io.Writer, payload []byte) error {
	return writeFrameLimit(w, payload, MaxFrameBytes)
}

// ReadFrame reads one bounded length-prefixed broker payload.
func ReadFrame(r io.Reader) ([]byte, error) {
	return readFrameLimit(r, MaxFrameBytes)
}

func writeFrameLimit(w io.Writer, payload []byte, max int) error {
	if w == nil || len(payload) == 0 {
		return ErrFrame
	}
	if len(payload) > max || uint64(len(payload)) > uint64(^uint32(0)) {
		return ErrFrameLimit
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload))) // #nosec G115 -- length is bounded by uint32 max above
	if err := writeFull(w, header[:]); err != nil {
		return redactWireError(ErrFrame, err)
	}
	if err := writeFull(w, payload); err != nil {
		return redactWireError(ErrFrame, err)
	}
	return nil
}

func readFrameLimit(r io.Reader, max int) ([]byte, error) {
	if r == nil {
		return nil, ErrFrame
	}
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, redactWireError(ErrFrame, err)
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 {
		return nil, ErrFrame
	}
	// #nosec G115 -- max is a positive, validated frame bound at every call site.
	if uint64(length) > uint64(max) {
		return nil, ErrFrameLimit
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, redactWireError(ErrFrame, err)
	}
	return payload, nil
}

func writeFull(w io.Writer, payload []byte) error {
	for len(payload) > 0 {
		n, err := w.Write(payload)
		if n < 0 || n > len(payload) {
			return io.ErrShortWrite
		}
		payload = payload[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copyOf := *value
	return &copyOf
}

type wireError struct {
	class error
	cause error
}

func (e *wireError) Error() string { return e.class.Error() }
func (e *wireError) Unwrap() error { return e.class }
func (e *wireError) Is(target error) bool {
	return target == e.class || (e.cause != nil && errors.Is(e.cause, target))
}

func redactWireError(class, cause error) error {
	if cause == nil {
		return class
	}
	return &wireError{class: class, cause: cause}
}
