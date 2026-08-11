package gmicloud

import "github.com/looprig/llm/providers/internal/simple"

func WithHeader(name, value string) Option { return simple.WithHeader(name, value) }

// WithOrganizationID selects the GMI organization for multi-organization API
// keys, as documented by GMI Cloud.
func WithOrganizationID(id string) Option { return simple.WithHeader("X-Organization-ID", id) }

// WithTopK forwards GMI Cloud's documented top-k sampling parameter. Support
// remains model-dependent on the upstream service.
func WithTopK(value int) Option { return simple.WithBodyField("top_k", value) }
