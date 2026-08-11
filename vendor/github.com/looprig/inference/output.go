package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	StructuredOutputToolName = "_looprig_final_output"
	StructuredOutputRevision = "structured-output/v1"

	maxOutputNameBytes        = 64
	maxOutputDescriptionBytes = 4096
	maxOutputSchemaBytes      = 1 << 20
	maxOutputSchemaDepth      = 64
	maxOutputSchemaProperties = 1024
)

// OutputSchema is a provider-neutral request for one schema-constrained JSON
// object. Description must be valid UTF-8 and is limited to 4096 bytes. Schema
// must satisfy the portable subset checked by ValidateOutputSchema.
type OutputSchema struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Strict      bool
}

// Clone returns an independent copy of the output schema.
func (o OutputSchema) Clone() OutputSchema {
	clone := o
	if o.Schema != nil {
		clone.Schema = make(json.RawMessage, len(o.Schema))
		copy(clone.Schema, o.Schema)
	}
	return clone
}

// ValidateOutputSchema validates output against the bounded portable JSON
// Schema subset shared by provider codecs.
func ValidateOutputSchema(output OutputSchema) error {
	if err := validateOutputName(output.Name); err != nil {
		return err
	}
	if !utf8.ValidString(output.Description) {
		return schemaError(SchemaFieldDescription, SchemaReasonInvalidUTF8)
	}
	if len(output.Description) > maxOutputDescriptionBytes {
		return schemaError(SchemaFieldDescription, SchemaReasonTooLong)
	}
	if len(output.Schema) > maxOutputSchemaBytes {
		return schemaError(SchemaFieldSchema, SchemaReasonTooLarge)
	}
	if !utf8.Valid(output.Schema) || !json.Valid(output.Schema) {
		return schemaError(SchemaFieldSchema, SchemaReasonMalformed)
	}
	if firstJSONByte(output.Schema) != '{' {
		return schemaError(SchemaFieldSchema, SchemaReasonRootNotObject)
	}
	duplicateKind, duplicate, err := findDuplicateObjectMember(output.Schema, true)
	if err != nil {
		return schemaError(SchemaFieldSchema, SchemaReasonMalformed)
	}
	if duplicate {
		field := SchemaFieldKeyword
		if duplicateKind == duplicateMemberSchemaProperty {
			field = SchemaFieldProperties
		}
		return schemaError(field, SchemaReasonDuplicate)
	}

	validator := schemaValidator{}
	return validator.validateNode(output.Schema, 1, true)
}

func validateOutputName(name string) error {
	if name == "" {
		return schemaError(SchemaFieldName, SchemaReasonEmpty)
	}
	if len(name) > maxOutputNameBytes {
		return schemaError(SchemaFieldName, SchemaReasonTooLong)
	}
	if name == StructuredOutputToolName {
		return schemaError(SchemaFieldName, SchemaReasonReserved)
	}
	for i := range len(name) {
		ch := name[i]
		if i == 0 {
			if !isASCIIAlpha(ch) && ch != '_' {
				return schemaError(SchemaFieldName, SchemaReasonInvalid)
			}
			continue
		}
		if !isASCIIAlpha(ch) && !isASCIIDigit(ch) && ch != '_' && ch != '-' {
			return schemaError(SchemaFieldName, SchemaReasonInvalid)
		}
	}
	return nil
}

func isASCIIAlpha(ch byte) bool {
	return ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z'
}

func isASCIIDigit(ch byte) bool {
	return ch >= '0' && ch <= '9'
}

type schemaNode struct {
	Type                 schemaStringValue     `json:"type"`
	Description          schemaStringValue     `json:"description"`
	Properties           schemaPropertiesValue `json:"properties"`
	Items                schemaRawValue        `json:"items"`
	Enum                 schemaEnumValue       `json:"enum"`
	Required             schemaStringsValue    `json:"required"`
	AdditionalProperties schemaBoolValue       `json:"additionalProperties"`
}

type schemaStringValue struct {
	present bool
	valid   bool
	value   string
}

func (v *schemaStringValue) UnmarshalJSON(raw []byte) error {
	v.present = true
	v.valid = firstJSONByte(raw) == '"' && json.Unmarshal(raw, &v.value) == nil && utf8.ValidString(v.value)
	return nil
}

type schemaPropertiesValue struct {
	present bool
	valid   bool
	values  map[string]json.RawMessage
}

func (v *schemaPropertiesValue) UnmarshalJSON(raw []byte) error {
	v.present = true
	v.valid = firstJSONByte(raw) == '{' && json.Unmarshal(raw, &v.values) == nil && v.values != nil
	return nil
}

type schemaRawValue struct {
	present bool
	value   json.RawMessage
}

func (v *schemaRawValue) UnmarshalJSON(raw []byte) error {
	v.present = true
	v.value = append(v.value[:0], raw...)
	return nil
}

type schemaEnumValue struct {
	present bool
	valid   bool
	values  []json.RawMessage
}

func (v *schemaEnumValue) UnmarshalJSON(raw []byte) error {
	v.present = true
	v.valid = firstJSONByte(raw) == '[' && json.Unmarshal(raw, &v.values) == nil && v.values != nil
	return nil
}

type schemaStringsValue struct {
	present bool
	valid   bool
	values  []string
}

func (v *schemaStringsValue) UnmarshalJSON(raw []byte) error {
	v.present = true
	var members []json.RawMessage
	if firstJSONByte(raw) != '[' || json.Unmarshal(raw, &members) != nil || members == nil {
		v.valid = false
		return nil
	}
	v.values = make([]string, 0, len(members))
	for _, member := range members {
		var value string
		if firstJSONByte(member) != '"' || json.Unmarshal(member, &value) != nil {
			v.values = nil
			v.valid = false
			return nil
		}
		v.values = append(v.values, value)
	}
	v.valid = true
	return nil
}

type schemaBoolValue struct {
	present bool
	valid   bool
	value   bool
}

func (v *schemaBoolValue) UnmarshalJSON(raw []byte) error {
	v.present = true
	trimmed := bytes.TrimSpace(raw)
	v.valid = bytes.Equal(trimmed, []byte("true")) || bytes.Equal(trimmed, []byte("false"))
	if v.valid {
		v.value = bytes.Equal(trimmed, []byte("true"))
	}
	return nil
}

type schemaProperty struct {
	name   string
	schema json.RawMessage
}

type schemaValidator struct {
	propertyCount int
}

func (v *schemaValidator) validateNode(raw json.RawMessage, depth int, root bool) error {
	if depth > maxOutputSchemaDepth {
		return schemaError(SchemaFieldSchema, SchemaReasonTooDeep)
	}
	if firstJSONByte(raw) != '{' {
		return schemaError(SchemaFieldSchema, SchemaReasonInvalid)
	}

	node, err := decodeSchemaNode(raw)
	if err != nil {
		return err
	}
	if !node.Type.present {
		return schemaError(SchemaFieldType, SchemaReasonMissing)
	}
	if !node.Type.valid || node.Type.value == "" {
		return schemaError(SchemaFieldType, SchemaReasonInvalid)
	}
	schemaType := node.Type.value
	if root && schemaType != "object" {
		return schemaError(SchemaFieldSchema, SchemaReasonRootNotObject)
	}
	if node.Description.present {
		if !node.Description.valid {
			return schemaError(SchemaFieldDescription, SchemaReasonInvalid)
		}
		if len(node.Description.value) > maxOutputDescriptionBytes {
			return schemaError(SchemaFieldDescription, SchemaReasonTooLong)
		}
	}

	switch schemaType {
	case "object":
		return v.validateObject(node, depth)
	case "array":
		return v.validateArray(node, depth)
	case "string", "boolean", "number", "integer":
		return validateScalar(node, schemaType)
	default:
		return schemaError(SchemaFieldType, SchemaReasonUnsupported)
	}
}

func (v *schemaValidator) validateObject(node schemaNode, depth int) error {
	if node.Items.present {
		return schemaError(SchemaFieldItems, SchemaReasonUnsupported)
	}
	if node.Enum.present {
		return schemaError(SchemaFieldEnum, SchemaReasonUnsupported)
	}
	if !node.AdditionalProperties.present {
		return schemaError(SchemaFieldAdditionalProperties, SchemaReasonMissing)
	}
	if !node.AdditionalProperties.valid || node.AdditionalProperties.value {
		return schemaError(SchemaFieldAdditionalProperties, SchemaReasonMustBeFalse)
	}

	if node.Properties.present && !node.Properties.valid {
		return schemaError(SchemaFieldProperties, SchemaReasonInvalid)
	}
	properties := make([]schemaProperty, 0, len(node.Properties.values))
	for name, schema := range node.Properties.values {
		properties = append(properties, schemaProperty{name: name, schema: schema})
	}
	v.propertyCount += len(properties)
	if v.propertyCount > maxOutputSchemaProperties {
		return schemaError(SchemaFieldProperties, SchemaReasonTooManyProperties)
	}

	if !node.Required.present {
		if len(properties) > 0 {
			return schemaError(SchemaFieldRequired, SchemaReasonMissing)
		}
	} else if !node.Required.valid {
		return schemaError(SchemaFieldRequired, SchemaReasonInvalid)
	}
	known := make(map[string]struct{}, len(properties))
	for _, property := range properties {
		known[property.name] = struct{}{}
	}
	seenRequired := make(map[string]struct{}, len(node.Required.values))
	for _, name := range node.Required.values {
		if _, duplicate := seenRequired[name]; duplicate {
			return schemaError(SchemaFieldRequired, SchemaReasonDuplicate)
		}
		seenRequired[name] = struct{}{}
		if _, exists := known[name]; !exists {
			return schemaError(SchemaFieldRequired, SchemaReasonUnknownProperty)
		}
	}
	if len(seenRequired) != len(known) {
		return schemaError(SchemaFieldRequired, SchemaReasonMissing)
	}

	for _, property := range properties {
		if err := v.validateNode(property.schema, depth+1, false); err != nil {
			return err
		}
	}
	return nil
}

func (v *schemaValidator) validateArray(node schemaNode, depth int) error {
	if node.Properties.present {
		return schemaError(SchemaFieldProperties, SchemaReasonUnsupported)
	}
	if node.Required.present {
		return schemaError(SchemaFieldRequired, SchemaReasonUnsupported)
	}
	if node.AdditionalProperties.present {
		return schemaError(SchemaFieldAdditionalProperties, SchemaReasonUnsupported)
	}
	if node.Enum.present {
		return schemaError(SchemaFieldEnum, SchemaReasonUnsupported)
	}
	if !node.Items.present {
		return schemaError(SchemaFieldItems, SchemaReasonMissing)
	}
	if firstJSONByte(node.Items.value) != '{' {
		return schemaError(SchemaFieldItems, SchemaReasonInvalid)
	}
	return v.validateNode(node.Items.value, depth+1, false)
}

func validateScalar(node schemaNode, schemaType string) error {
	if node.Properties.present {
		return schemaError(SchemaFieldProperties, SchemaReasonUnsupported)
	}
	if node.Items.present {
		return schemaError(SchemaFieldItems, SchemaReasonUnsupported)
	}
	if node.Required.present {
		return schemaError(SchemaFieldRequired, SchemaReasonUnsupported)
	}
	if node.AdditionalProperties.present {
		return schemaError(SchemaFieldAdditionalProperties, SchemaReasonUnsupported)
	}
	if !node.Enum.present {
		return nil
	}

	if !node.Enum.valid || len(node.Enum.values) == 0 {
		return schemaError(SchemaFieldEnum, SchemaReasonInvalid)
	}
	for _, value := range node.Enum.values {
		if !enumValueMatches(value, schemaType) {
			return schemaError(SchemaFieldEnum, SchemaReasonTypeMismatch)
		}
	}
	return nil
}

func enumValueMatches(raw json.RawMessage, schemaType string) bool {
	switch schemaType {
	case "string":
		var value string
		return firstJSONByte(raw) == '"' && json.Unmarshal(raw, &value) == nil
	case "boolean":
		var value bool
		return json.Unmarshal(raw, &value) == nil && (bytes.Equal(raw, []byte("true")) || bytes.Equal(raw, []byte("false")))
	case "number", "integer":
		var value json.Number
		if json.Unmarshal(raw, &value) != nil || value == "" {
			return false
		}
		return schemaType == "number" || jsonNumberIsIntegral(value.String())
	default:
		return false
	}
}

func jsonNumberIsIntegral(number string) bool {
	mantissa := number
	exponent := 0
	if index := strings.IndexAny(mantissa, "eE"); index >= 0 {
		parsed, err := strconv.Atoi(mantissa[index+1:])
		if err != nil {
			return false
		}
		exponent = parsed
		mantissa = mantissa[:index]
	}
	mantissa = strings.TrimPrefix(mantissa, "-")
	fractionDigits := 0
	if index := strings.IndexByte(mantissa, '.'); index >= 0 {
		fractionDigits = len(mantissa) - index - 1
		mantissa = mantissa[:index] + mantissa[index+1:]
	}
	remainingFraction := fractionDigits - exponent
	if remainingFraction <= 0 {
		return true
	}
	if remainingFraction > len(mantissa) {
		return strings.Trim(mantissa, "0") == ""
	}
	return strings.Trim(mantissa[len(mantissa)-remainingFraction:], "0") == ""
}

func decodeSchemaNode(raw json.RawMessage) (schemaNode, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var node schemaNode
	if err := decoder.Decode(&node); err != nil {
		return schemaNode{}, schemaError(SchemaFieldKeyword, SchemaReasonUnknownKeyword)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return schemaNode{}, schemaError(SchemaFieldSchema, SchemaReasonInvalid)
	}
	return node, nil
}

func firstJSONByte(raw json.RawMessage) byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0
	}
	return trimmed[0]
}

func schemaError(field SchemaValidationField, reason SchemaValidationReason) error {
	return &SchemaValidationError{Field: field, ReasonCode: reason}
}
