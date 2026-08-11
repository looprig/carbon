package model

// ModelKey is the stable, secret-free identity of a resolved model. It contains
// only the provider namespace and provider model ID, so identity never depends
// on a mutable catalog, endpoint, or wire format.
type ModelKey struct {
	Provider ProviderName
	Model    string
}

// ModelKeyField identifies a component of ModelKey.
type ModelKeyField string

const (
	ModelKeyFieldProvider ModelKeyField = "Provider"
	ModelKeyFieldModel    ModelKeyField = "Model"
)

// ModelKeyValidationReason identifies why a ModelKey is invalid.
type ModelKeyValidationReason string

const ModelKeyValidationReasonEmpty ModelKeyValidationReason = "must not be empty"

// ModelKeyValidationError reports an invalid ModelKey component.
type ModelKeyValidationError struct {
	Field  ModelKeyField
	Reason ModelKeyValidationReason
}

func (e *ModelKeyValidationError) Error() string {
	return "inference: invalid model key field " + string(e.Field) + ": " + string(e.Reason)
}

// Validate verifies that both identity components are known.
func (k ModelKey) Validate() error {
	if k.Provider == "" {
		return &ModelKeyValidationError{
			Field:  ModelKeyFieldProvider,
			Reason: ModelKeyValidationReasonEmpty,
		}
	}
	if k.Model == "" {
		return &ModelKeyValidationError{
			Field:  ModelKeyFieldModel,
			Reason: ModelKeyValidationReasonEmpty,
		}
	}
	return nil
}
