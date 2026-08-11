package cloudflareworkers

type ConfigurationReason string

const AccountMissing ConfigurationReason = "account ID is missing"

type ConfigurationError struct {
	Reason ConfigurationReason
}

func (e *ConfigurationError) Error() string {
	return "cloudflare-workers-ai: configuration: " + string(e.Reason)
}
