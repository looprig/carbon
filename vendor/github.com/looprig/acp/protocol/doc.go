// Package protocol is the pure wire layer of the Agent Client Protocol
// bridge: JSON-RPC framing, ACP message types, and generated protocol
// vocabulary. It never imports github.com/looprig/harness or
// github.com/looprig/core, directly or transitively (see acp/CLAUDE.md).
//
// types_gen.go and methods_gen.go are generated from the pinned schema
// artifacts in schema/v1/ by internal/gen. Regenerate with:
//
//go:generate go run ../internal/gen -schema schema/v1/schema.json -meta schema/v1/meta.json -out .
package protocol
