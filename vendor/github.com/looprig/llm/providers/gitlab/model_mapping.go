package gitlab

import (
	"fmt"
	"strings"

	model "github.com/looprig/inference/model"
)

type modelMapping struct {
	ID     string
	Format model.APIFormat
}

// This table mirrors GitLab Duo's current direct-access model mapping. The
// user-facing Duo aliases are deliberately not sent to the upstream APIs.
var modelMappings = map[string]modelMapping{
	"duo-chat-opus-4-6":      {ID: "claude-opus-4-6", Format: model.APIFormatAnthropic},
	"duo-chat-sonnet-4-6":    {ID: "claude-sonnet-4-6", Format: model.APIFormatAnthropic},
	"duo-chat-opus-4-5":      {ID: "claude-opus-4-5-20251101", Format: model.APIFormatAnthropic},
	"duo-chat-sonnet-4-5":    {ID: "claude-sonnet-4-5-20250929", Format: model.APIFormatAnthropic},
	"duo-chat-haiku-4-5":     {ID: "claude-haiku-4-5-20251001", Format: model.APIFormatAnthropic},
	"duo-chat-gpt-5-1":       {ID: "gpt-5.1-2025-11-13", Format: model.APIFormatOpenAI},
	"duo-chat-gpt-5-2":       {ID: "gpt-5.2-2025-12-11", Format: model.APIFormatOpenAI},
	"duo-chat-gpt-5-mini":    {ID: "gpt-5-mini-2025-08-07", Format: model.APIFormatOpenAI},
	"duo-chat-gpt-5-codex":   {ID: "gpt-5-codex", Format: model.APIFormatOpenAIResponses},
	"duo-chat-gpt-5-2-codex": {ID: "gpt-5.2-codex", Format: model.APIFormatOpenAIResponses},
	"duo-chat-gpt-5-3-codex": {ID: "gpt-5.3-codex", Format: model.APIFormatOpenAIResponses},
}

type modelOverride struct {
	ID     string
	Format model.APIFormat
}

func resolveModel(name string, format model.APIFormat, override *modelOverride) (string, error) {
	if override != nil {
		if strings.TrimSpace(override.ID) == "" {
			return "", &ModelMappingError{Alias: name, Format: format, Reason: "upstream model ID is empty"}
		}
		if override.Format != format {
			return "", &ModelMappingError{Alias: name, Format: format, Reason: fmt.Sprintf("explicit upstream model override belongs to %q", override.Format)}
		}
		return strings.TrimSpace(override.ID), nil
	}
	mapping, ok := modelMappings[name]
	if !ok {
		return "", &ModelMappingError{Alias: name, Format: format, Reason: "unknown GitLab model alias; use WithUpstreamModelID for a documented raw ID"}
	}
	if mapping.Format != format {
		return "", &ModelMappingError{Alias: name, Format: format, Reason: fmt.Sprintf("alias is documented for %q", mapping.Format)}
	}
	return mapping.ID, nil
}
