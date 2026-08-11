package azurecognitive

// ResourceConfigurationReason classifies Azure resource resolution failures.
type ResourceConfigurationReason string

const (
	ResourceMissing ResourceConfigurationReason = "resource name is missing"
	ResourceInvalid ResourceConfigurationReason = "resource name is invalid"
)

// ResourceConfigurationError is returned before network I/O for a missing or
// malformed Azure resource name.
type ResourceConfigurationError struct {
	Reason ResourceConfigurationReason
}

func (e *ResourceConfigurationError) Error() string {
	return "azure cognitive services: " + string(e.Reason)
}

func validResource(resource string) bool {
	if len(resource) == 0 || len(resource) > 64 || resource[0] == '-' || resource[len(resource)-1] == '-' {
		return false
	}
	for _, char := range resource {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}
