package hustle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

const (
	// MaxEvidenceToolDescriptionBytes bounds each concrete tool description
	// by encoded UTF-8 bytes before the metadata is retained or fingerprinted.
	MaxEvidenceToolDescriptionBytes = 4 << 10
	// MaxEvidenceToolSchemaBytes bounds each concrete tool's raw JSON schema
	// before validation and whitespace compaction.
	MaxEvidenceToolSchemaBytes = 1 << 20
	// MaxEvidenceToolMetadataBytes bounds the aggregate model-facing concrete
	// tool names, descriptions, and compact schemas in one bound catalog.
	MaxEvidenceToolMetadataBytes = 4 << 20
)

// BoundEvidenceTool is an immutable, fingerprinted evidence capability. Its
// model-facing metadata is frozen separately from the concrete execution tool
// so optional capabilities on that tool remain available to the runtime.
type BoundEvidenceTool struct{ state *boundEvidenceToolState }

type boundEvidenceToolState struct {
	tool              tool.InvokableTool
	info              tool.ToolInfo
	name              string
	descriptionSHA256 [sha256.Size]byte
	schemaSHA256      [sha256.Size]byte
	identitySHA256    [sha256.Size]byte
}

func (b BoundEvidenceTool) Name() string {
	if b.state == nil {
		return ""
	}
	return b.state.name
}

func (b BoundEvidenceTool) DescriptionSHA256() [sha256.Size]byte {
	if b.state == nil {
		return [sha256.Size]byte{}
	}
	return b.state.descriptionSHA256
}

func (b BoundEvidenceTool) SchemaSHA256() [sha256.Size]byte {
	if b.state == nil {
		return [sha256.Size]byte{}
	}
	return b.state.schemaSHA256
}

func (b BoundEvidenceTool) IdentitySHA256() [sha256.Size]byte {
	if b.state == nil {
		return [sha256.Size]byte{}
	}
	return b.state.identitySHA256
}

func (b BoundEvidenceTool) Tool() tool.InvokableTool {
	if b.state == nil {
		return nil
	}
	return b.state.tool
}

// Info returns a defensive copy of the exact metadata frozen at bind time.
// Execution uses Tool so optional capabilities on the concrete tool are
// preserved; runtimes must use this accessor for model-facing metadata.
func (b BoundEvidenceTool) Info() *tool.ToolInfo {
	if b.state == nil {
		return nil
	}
	clone := b.state.info
	clone.Schema = append(json.RawMessage(nil), b.state.info.Schema...)
	return &clone
}

func bindEvidenceTools(
	ctx context.Context,
	policy EvidenceToolPolicy,
	bindings EvidenceBindings,
) ([]BoundEvidenceTool, error) {
	if ctx == nil {
		return nil, &BindError{Kind: BindInvalidContext}
	}
	if !evidencePolicyEnabled(policy) {
		return nil, nil
	}
	if bindings.SessionID.IsZero() || bindings.LoopID.IsZero() {
		return nil, invalidEvidenceBind(nil)
	}
	if len(policy.Definitions) == 0 || len(policy.Definitions) > MaxEvidenceToolDefinitions {
		return nil, invalidEvidenceBind(nil)
	}
	built := make([]tool.InvokableTool, 0)
	declared := make([]string, 0)
	staticInfos := make([]tool.ToolInfo, 0)
	for _, definition := range policy.Definitions {
		if nilToolDefinition(definition) {
			return nil, invalidEvidenceBind(nil)
		}
		names := definition.ProducedToolNames()
		infos := definition.ToolInfos()
		if len(names) == 0 || len(infos) != len(names) ||
			len(names) > MaxEvidenceProducedToolNames-len(declared) {
			return nil, invalidEvidenceBind(nil)
		}
		for index, name := range names {
			if !canonicalEvidenceToolName(name) || infos[index].Name != name {
				return nil, invalidEvidenceBind(nil)
			}
			canonicalSchema, err := validateEvidenceToolInfo(infos[index])
			if err != nil {
				return nil, invalidEvidenceBind(err)
			}
			infos[index].Schema = canonicalSchema
		}
		declared = append(declared, names...)
		staticInfos = append(staticInfos, infos...)
		toolBindings := tool.Bindings{
			SessionID: bindings.SessionID,
			LoopID:    bindings.LoopID,
		}
		if definition.Requirements()&tool.RequiresWorkspaceRead != 0 {
			if bindings.ReadWorkspace == nil {
				return nil, invalidEvidenceBind(nil)
			}
			toolBindings.ReadWorkspace = &tool.ReadWorkspaceBinding{Root: bindings.ReadWorkspace.Root}
		}
		tools, err := definition.Build(ctx, toolBindings)
		if err != nil {
			return nil, invalidEvidenceBind(err)
		}
		if len(tools) > MaxEvidenceProducedToolNames-len(built) {
			return nil, invalidEvidenceBind(nil)
		}
		built = append(built, tools...)
	}
	if len(built) == 0 || len(built) > MaxEvidenceProducedToolNames || len(built) != len(declared) {
		return nil, invalidEvidenceBind(nil)
	}
	result := make([]BoundEvidenceTool, len(built))
	seen := make(map[string]struct{}, len(built))
	metadataBytes := 0
	for index, concrete := range built {
		if nilEvidenceTool(concrete) {
			return nil, invalidEvidenceBind(nil)
		}
		info, err := concrete.Info(ctx)
		if err != nil || info == nil {
			return nil, invalidEvidenceBind(err)
		}
		canonicalSchema, err := validateEvidenceToolInfo(*info)
		if err != nil {
			return nil, invalidEvidenceBind(err)
		}
		confirmed, err := concrete.Info(ctx)
		if err != nil || confirmed == nil {
			return nil, invalidEvidenceBind(err)
		}
		confirmedSchema, err := validateEvidenceToolInfo(*confirmed)
		if err != nil ||
			info.Name != confirmed.Name ||
			info.Desc != confirmed.Desc ||
			!bytes.Equal(canonicalSchema, confirmedSchema) {
			return nil, invalidEvidenceBind(err)
		}
		if info.Name != declared[index] || !canonicalEvidenceToolName(info.Name) {
			return nil, invalidEvidenceBind(nil)
		}
		expected := staticInfos[index]
		if info.Name != expected.Name ||
			info.Desc != expected.Desc ||
			!bytes.Equal(canonicalSchema, expected.Schema) {
			return nil, invalidEvidenceBind(nil)
		}
		if _, duplicate := seen[info.Name]; duplicate {
			return nil, invalidEvidenceBind(nil)
		}
		seen[info.Name] = struct{}{}
		var withinLimit bool
		metadataBytes, withinLimit = addEvidenceMetadataBytes(metadataBytes, len(info.Name))
		if withinLimit {
			metadataBytes, withinLimit = addEvidenceMetadataBytes(metadataBytes, len(info.Desc))
		}
		if withinLimit {
			metadataBytes, withinLimit = addEvidenceMetadataBytes(metadataBytes, len(canonicalSchema))
		}
		if !withinLimit {
			return nil, invalidEvidenceBind(nil)
		}
		frozenInfo := tool.ToolInfo{
			Name: info.Name, Desc: info.Desc,
			Schema: append(json.RawMessage(nil), canonicalSchema...),
		}
		descriptionDigest := sha256.Sum256([]byte(info.Desc))
		schemaDigest := sha256.Sum256(canonicalSchema)
		identity, err := digestBoundEvidenceTool(info.Name, info.Desc, canonicalSchema)
		if err != nil {
			return nil, invalidEvidenceBind(err)
		}
		result[index] = BoundEvidenceTool{state: &boundEvidenceToolState{
			tool: concrete, info: frozenInfo, name: info.Name,
			descriptionSHA256: descriptionDigest,
			schemaSHA256:      schemaDigest,
			identitySHA256:    identity,
		}}
	}
	return result, nil
}

func validateEvidenceToolInfo(info tool.ToolInfo) ([]byte, error) {
	if !canonicalEvidenceToolName(info.Name) ||
		!utf8.ValidString(info.Desc) ||
		info.Desc != strings.TrimSpace(info.Desc) ||
		info.Desc == "" ||
		strings.ContainsRune(info.Desc, '\x00') ||
		len(info.Desc) > MaxEvidenceToolDescriptionBytes ||
		len(info.Schema) == 0 ||
		len(info.Schema) > MaxEvidenceToolSchemaBytes ||
		!utf8.Valid(info.Schema) {
		return nil, invalidEvidenceBind(nil)
	}
	if err := inference.ValidateOutputSchema(inference.OutputSchema{
		Name: info.Name, Description: info.Desc, Schema: info.Schema, Strict: true,
	}); err != nil {
		return nil, invalidEvidenceBind(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, info.Schema); err != nil {
		return nil, err
	}
	return append([]byte(nil), compact.Bytes()...), nil
}

func addEvidenceMetadataBytes(total, size int) (int, bool) {
	if total < 0 || size < 0 || total > MaxEvidenceToolMetadataBytes ||
		size > MaxEvidenceToolMetadataBytes-total {
		return 0, false
	}
	return total + size, true
}

func digestBoundEvidenceTool(name, description string, schema []byte) ([sha256.Size]byte, error) {
	if !canonicalEvidenceToolName(name) || !utf8.ValidString(description) || !utf8.Valid(schema) {
		return [sha256.Size]byte{}, invalidEvidenceBind(nil)
	}
	material := appendCanonicalString(nil, boundEvidenceToolDigestDomain)
	material = appendCanonicalString(material, name)
	material = appendCanonicalString(material, description)
	material = appendCanonicalBytes(material, schema)
	return sha256.Sum256(material), nil
}

func nilEvidenceTool(value tool.InvokableTool) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func invalidEvidenceBind(cause error) error {
	return &BindError{Kind: BindInvalidEvidenceTools, Cause: cause}
}
