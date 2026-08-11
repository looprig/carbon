package sap

import (
	"fmt"

	"github.com/looprig/llm"
)

type ConfigurationReason string

const (
	ServiceKeyMissing  ConfigurationReason = "service key is missing"
	InvalidServiceKey  ConfigurationReason = "service key is invalid"
	InvalidModelParams ConfigurationReason = "model parameters are invalid"
	DeploymentMissing  ConfigurationReason = "no running orchestration deployment was found"
)

type ConfigurationError struct {
	Reason ConfigurationReason
	Err    error
}

func (e *ConfigurationError) Error() string {
	if e.Err != nil {
		return "sap-ai-core: configuration: " + string(e.Reason) + ": " + e.Err.Error()
	}
	return "sap-ai-core: configuration: " + string(e.Reason)
}

func (e *ConfigurationError) Unwrap() error { return e.Err }

type AuthError struct {
	Status int
	Err    error
}

func (e *AuthError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("sap-ai-core: token request failed with status %d", e.Status)
	}
	return fmt.Sprintf("sap-ai-core: token request failed: %v", e.Err)
}

func (e *AuthError) Unwrap() error { return e.Err }

type RequestError struct {
	Status int
	Err    error
}

func (e *RequestError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("sap-ai-core: deployment discovery failed with status %d", e.Status)
	}
	return fmt.Sprintf("sap-ai-core: deployment discovery failed: %v", e.Err)
}

func (e *RequestError) Unwrap() error { return e.Err }

type CounterSupportError = llm.CounterSupportError
