package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"

	"github.com/looprig/harness/pkg/tool"
)

const prepareErrorText = "invalid task arguments"

// prepareError is the private, stable error returned for all task argument
// preparation failures. Its text is intentionally generic: model-supplied
// task text, metadata, and arbitrary argument values must not cross this
// boundary into a surfaced error.
type prepareError struct{}

func (*prepareError) Error() string { return prepareErrorText }

// objectFields preserves field presence independently of the zero value a
// typed decoder assigns to a struct field. A key with JSON false, 0, "", or
// null is present; an absent key is not.
type objectFields map[string]json.RawMessage

func (fields objectFields) has(name string) bool {
	_, ok := fields[name]
	return ok
}

// decodeObject bounds, validates, and decodes one task argument object. The
// returned field set is the presence-aware view used by concrete task tools;
// target receives the same document through encoding/json's strict decoder.
func decodeObject(raw string, target any) (objectFields, error) {
	if len(raw) > maxTaskArgsBytes {
		return nil, &prepareError{}
	}

	fields, err := scanObject([]byte(raw), false)
	if err != nil {
		return nil, &prepareError{}
	}

	if target != nil {
		if allowed, ok := exactJSONFieldNames(target); ok {
			for key := range fields {
				if _, known := allowed[key]; !known {
					return nil, &prepareError{}
				}
			}
		}

		decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			return nil, &prepareError{}
		}
		if err := requireEOF(decoder); err != nil {
			return nil, &prepareError{}
		}
	}
	return fields, nil
}

// exactJSONFieldNames returns the top-level names encoding/json should accept
// for a struct target, using the declared JSON tag spelling rather than the
// decoder's case-insensitive matching. Embedded tagged structs are flattened
// just as encoding/json flattens them. Non-struct targets are left to the
// decoder because maps and custom scalar targets do not have a struct allowlist.
func exactJSONFieldNames(target any) (map[string]struct{}, bool) {
	typ := reflect.TypeOf(target)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return nil, false
	}

	names := make(map[string]struct{})
	collectJSONFieldNames(typ, names, make(map[reflect.Type]bool))
	return names, true
}

func collectJSONFieldNames(typ reflect.Type, names map[string]struct{}, visiting map[reflect.Type]bool) {
	if visiting[typ] {
		return
	}
	visiting[typ] = true
	defer delete(visiting, typ)

	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		tag, tagged := field.Tag.Lookup("json")
		if tagged && tag == "-" {
			continue
		}
		if field.PkgPath != "" && !field.Anonymous {
			continue
		}

		name := strings.Split(tag, ",")[0]
		if field.Anonymous && name == "" {
			nested := field.Type
			for nested.Kind() == reflect.Pointer {
				nested = nested.Elem()
			}
			if nested.Kind() == reflect.Struct {
				collectJSONFieldNames(nested, names, visiting)
				continue
			}
		}
		if name == "" {
			name = field.Name
		}
		if name != "" {
			names[name] = struct{}{}
		}
	}
}

// scanObject validates a single JSON object and captures its top-level raw
// field values. It rejects duplicate top-level members and trailing input.
// When rejectNestedDuplicates is true, nested objects are checked as well.
func scanObject(data []byte, rejectNestedDuplicates bool) (objectFields, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := first.(json.Delim)
	if !ok || delim != '{' {
		return nil, errors.New("JSON root is not an object")
	}

	fields := make(objectFields)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("JSON object member name is not a string")
		}
		if fields.has(key) {
			return nil, errors.New("duplicate JSON object member")
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if rejectNestedDuplicates {
			if err := validateJSONDocument(value); err != nil {
				return nil, err
			}
		}
		fields[key] = cloneRawMessage(value)
	}

	closing, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if closing != json.Delim('}') {
		return nil, errors.New("JSON object did not close")
	}
	if err := requireEOF(decoder); err != nil {
		return nil, err
	}
	return fields, nil
}

// validateJSONDocument consumes exactly one JSON value and rejects duplicate
// members at every object level. It is used for metadata, whose values may be
// nested but must still have an unambiguous JSON representation.
func validateJSONDocument(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	return requireEOF(decoder)
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}

	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object member name is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate JSON object member")
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("JSON object did not close")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("JSON array did not close")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func requireEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return errors.New("trailing JSON value")
	}
	return err
}

// metadataClear is the non-nil raw JSON sentinel passed to TaskUpdate for an
// explicit metadata clear. Omitted metadata remains a nil RawMessage.
var metadataClear = json.RawMessage(`{}`)

// canonicalMetadata validates and canonicalizes one metadata value. A nil
// input means the metadata field was omitted. A non-nil {} is returned as a
// separate, non-nil sentinel so TaskUpdate can distinguish clearing metadata
// from leaving it unchanged.
func canonicalMetadata(raw json.RawMessage) (json.RawMessage, error) {
	if raw == nil {
		return nil, nil
	}
	if len(raw) > maxMetadataBytes {
		return nil, &prepareError{}
	}

	fields, err := scanObject(raw, true)
	if err != nil {
		return nil, &prepareError{}
	}
	if len(fields) == 0 {
		return cloneRawMessage(metadataClear), nil
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, &prepareError{}
	}
	if err := requireEOF(decoder); err != nil {
		return nil, &prepareError{}
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, &prepareError{}
	}
	return cloneRawMessage(canonical), nil
}

// jsonResult marshals one structured result and uses the harness's canonical
// single-text-block result contract.
func jsonResult(value any) (*tool.ToolResult, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return tool.TextResult(string(encoded)), nil
}

// toolBase contains only the fields shared by concrete task tools. Concrete
// tools own their Info and PrepareCall implementations and their schemas.
type toolBase struct {
	name string
	desc string
}

func (b toolBase) AuditSummary(string) string { return b.name }

// NewTools constructs the four task tools over one private Loop-local store.
// The returned order is the order exposed by tools.TaskDefinitions.
func NewTools() []tool.InvokableTool {
	store := newStore(nil)
	return []tool.InvokableTool{
		newTaskCreate(store),
		newTaskUpdate(store),
		newTaskGet(store),
		newTaskList(store),
	}
}
