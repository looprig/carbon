package cloudflaregateway

type ConfigurationReason string

const (
	AccountMissing ConfigurationReason = "account ID is missing"
)

type ConfigurationError struct {
	Reason ConfigurationReason
}

func (e *ConfigurationError) Error() string {
	return "cloudflare-ai-gateway: configuration: " + string(e.Reason)
}
