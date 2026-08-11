package snowflake

import "github.com/looprig/llm"

type CounterSupportError = llm.CounterSupportError

type ConfigurationReason string

const (
	AccountMissing ConfigurationReason = "account is missing"
	AccountInvalid ConfigurationReason = "account is invalid"
)

type ConfigurationError struct {
	Reason ConfigurationReason
}

func (e *ConfigurationError) Error() string {
	return "snowflake-cortex: configuration: " + string(e.Reason)
}
