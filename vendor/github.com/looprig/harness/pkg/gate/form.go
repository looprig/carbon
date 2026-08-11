package gate

import (
	"encoding/json"
	"fmt"
)

// Form gate response actions. They are the Control.Action / ResponseRequest.Action
// values a form gate understands, and are answered through the ordinary
// ResponsePolicy machinery (a PolicyRespond template naming FormActionDecline is
// the fail-secure default an integration should configure for an unattended
// form).
const (
	// FormActionAccept submits answers; Values must satisfy ParseFormAnswers.
	FormActionAccept = "accept"
	// FormActionDecline records an explicit human refusal to answer.
	FormActionDecline = "decline"
	// FormActionCancel records that the request was withdrawn or timed out
	// rather than refused.
	FormActionCancel = "cancel"
)

// Form schema and answer bounds. A form is a human-facing prompt, not a data
// channel: these caps keep a hostile or buggy integration from journaling
// unbounded text or rendering an unanswerable prompt. They are enforced at both
// codec boundaries and when parsing a response.
const (
	// maxFormFields caps the number of fields in a form schema.
	maxFormFields = 32
	// maxFormFieldNameBytes caps a field name.
	maxFormFieldNameBytes = 128
	// maxFormFieldOptions caps the option list of a select field.
	maxFormFieldOptions = 64
	// maxFormValueBytes caps a single submitted answer.
	maxFormValueBytes = 4096
)

// FormSchemaErrorKind classifies a rejected form schema.
type FormSchemaErrorKind string

const (
	// FormSchemaEmpty reports a schema with no fields.
	FormSchemaEmpty FormSchemaErrorKind = "schema_empty"
	// FormSchemaTooManyFields reports more than maxFormFields fields.
	FormSchemaTooManyFields FormSchemaErrorKind = "schema_too_many_fields"
	// FormSchemaFieldNameEmpty reports a field with no name.
	FormSchemaFieldNameEmpty FormSchemaErrorKind = "field_name_empty"
	// FormSchemaFieldNameTooLong reports an over-long field name.
	FormSchemaFieldNameTooLong FormSchemaErrorKind = "field_name_too_long"
	// FormSchemaFieldNameDuplicate reports two fields sharing a name.
	FormSchemaFieldNameDuplicate FormSchemaErrorKind = "field_name_duplicate"
	// FormSchemaFieldKindUnsupported reports a field kind a form cannot answer.
	FormSchemaFieldKindUnsupported FormSchemaErrorKind = "field_kind_unsupported"
	// FormSchemaFieldOptionsInvalid reports a select field with no options, too
	// many options, or an empty option value.
	FormSchemaFieldOptionsInvalid FormSchemaErrorKind = "field_options_invalid"
)

// FormSchemaError reports a form schema that violates the form contract.
type FormSchemaError struct {
	Kind  FormSchemaErrorKind
	Field string
}

func (e *FormSchemaError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("gate: invalid form schema (%s)", string(e.Kind))
	}
	return fmt.Sprintf("gate: invalid form schema field %q (%s)", e.Field, string(e.Kind))
}

// FormAnswerErrorKind classifies a rejected form answer set.
type FormAnswerErrorKind string

const (
	// FormAnswerUnknownField reports a submitted value naming no schema field.
	FormAnswerUnknownField FormAnswerErrorKind = "unknown_field"
	// FormAnswerMissingRequired reports a required field with no value.
	FormAnswerMissingRequired FormAnswerErrorKind = "missing_required"
	// FormAnswerTypeInvalid reports a value whose JSON type does not match the
	// field kind (a string for text/select, a bool for confirm).
	FormAnswerTypeInvalid FormAnswerErrorKind = "type_invalid"
	// FormAnswerTooLong reports a value exceeding maxFormValueBytes.
	FormAnswerTooLong FormAnswerErrorKind = "too_long"
	// FormAnswerOptionNotAllowed reports a select value outside the field options.
	FormAnswerOptionNotAllowed FormAnswerErrorKind = "option_not_allowed"
)

// FormAnswerError reports a form response that does not satisfy its schema.
type FormAnswerError struct {
	Kind  FormAnswerErrorKind
	Field string
}

func (e *FormAnswerError) Error() string {
	return fmt.Sprintf("gate: invalid form answer for field %q (%s)", e.Field, string(e.Kind))
}

// formAnswerableKinds are the field kinds a form gate can request.
//
// FieldMultiSelect is deliberately EXCLUDED. A form answer is a
// map[string]string — one string per field — which cannot represent a
// multi-value selection without inventing an in-band separator that would be
// ambiguous against option values containing it. Rather than encode answers
// lossily or silently drop selections, a form schema that asks for one is
// rejected at every boundary. FieldMultiSelect remains valid in a Gate.Prompt
// for the existing kinds, which do not route through ParseFormAnswers.
func formAnswerableKind(kind FieldKind) bool {
	switch kind {
	case FieldText, FieldSelect, FieldConfirm:
		return true
	case FieldMultiSelect:
		return false
	default:
		return false
	}
}

// ValidateFormSchema reports whether a schema is a well-formed, bounded,
// answerable form request. It fails closed: an unknown field kind is rejected
// rather than treated as free text.
func ValidateFormSchema(schema PromptSchema) error {
	if len(schema.Fields) == 0 {
		return &FormSchemaError{Kind: FormSchemaEmpty}
	}
	if len(schema.Fields) > maxFormFields {
		return &FormSchemaError{Kind: FormSchemaTooManyFields}
	}
	seen := make(map[string]struct{}, len(schema.Fields))
	for _, field := range schema.Fields {
		if field.Name == "" {
			return &FormSchemaError{Kind: FormSchemaFieldNameEmpty}
		}
		if len(field.Name) > maxFormFieldNameBytes {
			return &FormSchemaError{Kind: FormSchemaFieldNameTooLong, Field: field.Name}
		}
		if _, dup := seen[field.Name]; dup {
			return &FormSchemaError{Kind: FormSchemaFieldNameDuplicate, Field: field.Name}
		}
		seen[field.Name] = struct{}{}
		if !formAnswerableKind(field.Kind) {
			return &FormSchemaError{Kind: FormSchemaFieldKindUnsupported, Field: field.Name}
		}
		if err := validateFormFieldOptions(field); err != nil {
			return err
		}
	}
	return nil
}

// validateFormFieldOptions enforces that a select field offers a usable,
// bounded option list and that a non-select field offers none.
func validateFormFieldOptions(field Field) error {
	if field.Kind != FieldSelect {
		if len(field.Options) > 0 {
			return &FormSchemaError{Kind: FormSchemaFieldOptionsInvalid, Field: field.Name}
		}
		return nil
	}
	if len(field.Options) == 0 || len(field.Options) > maxFormFieldOptions {
		return &FormSchemaError{Kind: FormSchemaFieldOptionsInvalid, Field: field.Name}
	}
	for _, option := range field.Options {
		if option.Value == "" {
			return &FormSchemaError{Kind: FormSchemaFieldOptionsInvalid, Field: field.Name}
		}
	}
	return nil
}

// ParseFormAnswers validates a FormActionAccept response's Values against the
// form schema and returns the answers keyed by field name.
//
// It is strict in both directions and fails closed: every submitted value must
// name a schema field, match that field's JSON type (a string for text and
// select, a bool for confirm), stay within maxFormValueBytes, and — for a select
// — name a declared option. Every required field must be present. A schema that
// does not satisfy ValidateFormSchema is rejected before any value is read, so a
// caller cannot smuggle an answer past an unvalidated field.
//
// A confirm answer is normalized to "true"/"false".
func ParseFormAnswers(schema PromptSchema, values map[string]json.RawMessage) (map[string]string, error) {
	if err := ValidateFormSchema(schema); err != nil {
		return nil, err
	}
	fields := make(map[string]Field, len(schema.Fields))
	for _, field := range schema.Fields {
		fields[field.Name] = field
	}
	for name := range values {
		if _, ok := fields[name]; !ok {
			return nil, &FormAnswerError{Kind: FormAnswerUnknownField, Field: name}
		}
	}
	answers := make(map[string]string, len(schema.Fields))
	for _, field := range schema.Fields {
		raw, ok := values[field.Name]
		if !ok || isExplicitJSONNull(raw) {
			if field.Required {
				return nil, &FormAnswerError{Kind: FormAnswerMissingRequired, Field: field.Name}
			}
			continue
		}
		answer, err := parseFormFieldAnswer(field, raw)
		if err != nil {
			return nil, err
		}
		answers[field.Name] = answer
	}
	return answers, nil
}

// parseFormFieldAnswer decodes and validates one field's raw JSON value.
func parseFormFieldAnswer(field Field, raw json.RawMessage) (string, error) {
	if field.Kind == FieldConfirm {
		var confirmed bool
		if err := json.Unmarshal(raw, &confirmed); err != nil {
			return "", &FormAnswerError{Kind: FormAnswerTypeInvalid, Field: field.Name}
		}
		if confirmed {
			return "true", nil
		}
		return "false", nil
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", &FormAnswerError{Kind: FormAnswerTypeInvalid, Field: field.Name}
	}
	if len(value) > maxFormValueBytes {
		return "", &FormAnswerError{Kind: FormAnswerTooLong, Field: field.Name}
	}
	if field.Required && value == "" {
		return "", &FormAnswerError{Kind: FormAnswerMissingRequired, Field: field.Name}
	}
	if field.Kind == FieldSelect && !formOptionAllowed(field, value) {
		return "", &FormAnswerError{Kind: FormAnswerOptionNotAllowed, Field: field.Name}
	}
	return value, nil
}

// FormAuditErrorKind classifies a rejected form audit record.
type FormAuditErrorKind string

const (
	// FormAuditTooManyValues reports more than maxFormFields answers.
	FormAuditTooManyValues FormAuditErrorKind = "too_many_values"
	// FormAuditFieldNameTooLong reports an over-long answer field name.
	FormAuditFieldNameTooLong FormAuditErrorKind = "field_name_too_long"
	// FormAuditValueTooLong reports an answer exceeding maxFormValueBytes.
	FormAuditValueTooLong FormAuditErrorKind = "value_too_long"
)

// FormAuditError reports a form audit whose contents exceed the durable bounds.
type FormAuditError struct {
	Kind  FormAuditErrorKind
	Field string
}

func (e *FormAuditError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("gate: invalid form audit (%s)", string(e.Kind))
	}
	return fmt.Sprintf("gate: invalid form audit field %q (%s)", e.Field, string(e.Kind))
}

// ValidateFormAuditBounds reports whether a FormAudit is small enough to journal.
//
// A form audit records user-authored content verbatim (see FormAudit), and the
// schema that solicited it is authored by a third party. Bounding it is therefore
// what keeps "the journal records what the human said" from becoming "a hostile
// integration can append whatever it likes to the journal". The bounds are the
// same ones ParseFormAnswers applies on the way in — an answer that passed it
// always passes this — and they are re-checked at both codec boundaries so a
// record that was never parsed (a forged or corrupted one) can be neither
// written nor read back.
func ValidateFormAuditBounds(audit FormAudit) error {
	if len(audit.Values) > maxFormFields {
		return &FormAuditError{Kind: FormAuditTooManyValues}
	}
	for name, value := range audit.Values {
		if len(name) > maxFormFieldNameBytes {
			return &FormAuditError{Kind: FormAuditFieldNameTooLong, Field: name}
		}
		if len(value) > maxFormValueBytes {
			return &FormAuditError{Kind: FormAuditValueTooLong, Field: name}
		}
	}
	return nil
}

// NewFormAudit builds the durable audit for answers that already satisfied
// ParseFormAnswers against schema.
//
// The schema drives the walk (not the answers map), so a field name the schema
// never declared cannot reach a durable record even if a caller puts one in
// answers.
func NewFormAudit(schema PromptSchema, answers map[string]string) FormAudit {
	audit := FormAudit{}
	for _, field := range schema.Fields {
		value, ok := answers[field.Name]
		if !ok {
			continue
		}
		if audit.Values == nil {
			audit.Values = make(map[string]string, len(answers))
		}
		audit.Values[field.Name] = value
	}
	return audit
}

func formOptionAllowed(field Field, value string) bool {
	for _, option := range field.Options {
		if option.Value == value {
			return true
		}
	}
	return false
}
