package model

// ProviderName is an opaque label identifying the backend a model belongs to.
// It carries no provider policy. An empty value is a wildcard, not a claim.
type ProviderName string
