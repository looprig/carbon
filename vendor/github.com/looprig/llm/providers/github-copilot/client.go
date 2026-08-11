// Package githubcopilot provides GitHub Copilot's documented OpenAI Chat and
// Responses gateway endpoints. The key passed to New is an already-authorized
// Copilot access token; OAuth device/browser exchange belongs in the caller's
// authentication layer and is intentionally not hidden in this transport.
package githubcopilot

import (
	"net/http"
	"os"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/compat"
	"github.com/looprig/llm/providers/internal/simple"
)

const (
	DefaultBaseURL = "https://api.githubcopilot.com"
	apiVersion     = "2026-06-01"
)

type Option = simple.Option

func WithHeader(name, value string) Option { return simple.WithHeader(name, value) }

func WithInitiator(value string) Option { return simple.WithHeader("x-initiator", value) }

func WithOpenAIIntent(value string) Option { return simple.WithHeader("Openai-Intent", value) }

func WithVision(enabled bool) Option {
	if enabled {
		return simple.WithHeader("Copilot-Vision-Request", "true")
	}
	return simple.WithHeader("Copilot-Vision-Request", "false")
}

func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	if key == "" {
		key = auth.APIKey(strings.TrimSpace(os.Getenv("GITHUB_COPILOT_TOKEN")))
	}
	defaults := []Option{
		simple.WithHeader("User-Agent", "looprig-llm-github-copilot"),
		simple.WithHeader("Openai-Intent", "conversation-edits"),
		simple.WithHeader("X-GitHub-Api-Version", apiVersion),
	}
	defaults = append(defaults, options...)
	definition := simple.Definition{
		Provider:          llm.ProviderGitHubCopilot,
		DefaultBaseURL:    DefaultBaseURL,
		Authentication:    auth.AuthAPIKey,
		PatchHeaders:      patchCopilotHeaders,
		NormalizeResponse: compat.NormalizeOpenAIReasoning,
		NormalizeStream:   compat.NormalizeOpenAIReasoningStream,
	}
	if selected.APIFormat == model.APIFormatOpenAIResponses {
		definition.DefaultPath = "/responses"
	} else if selected.APIFormat == model.APIFormatAnthropic {
		definition.DefaultPath = "/messages"
		defaults = append([]Option{
			simple.WithHeader("anthropic-version", "2023-06-01"),
			simple.WithHeader("anthropic-beta", "interleaved-thinking-2025-05-14"),
		}, defaults...)
	} else {
		definition.DefaultPath = "/chat/completions"
	}
	return simple.New(selected, key, definition, defaults...)
}

func patchCopilotHeaders(req inference.Request, headers http.Header) {
	if headers.Get("x-initiator") == "" {
		initiator := "agent"
		if lastIsUserPrompt(req.Messages) {
			initiator = "user"
		}
		headers.Set("x-initiator", initiator)
	}
	if headers.Get("Copilot-Vision-Request") == "" && messagesCarryImages(req.Messages) {
		headers.Set("Copilot-Vision-Request", "true")
	}
}

func lastIsUserPrompt(messages content.AgenticMessages) bool {
	if len(messages) == 0 {
		return false
	}
	last, ok := messages[len(messages)-1].(*content.UserMessage)
	if !ok || len(last.Blocks) == 0 {
		return false
	}
	for _, block := range last.Blocks {
		if _, isToolResult := block.(*content.ToolResultBlock); isToolResult {
			continue
		}
		if _, isImage := block.(*content.ImageBlock); isImage {
			return false
		}
		return true
	}
	return false
}

func messagesCarryImages(messages content.AgenticMessages) bool {
	for _, message := range messages {
		var blocks []content.Block
		switch message := message.(type) {
		case *content.SystemMessage:
			blocks = message.Blocks
		case *content.UserMessage:
			blocks = message.Blocks
		case *content.AIMessage:
			blocks = message.Blocks
		case *content.ToolResultMessage:
			blocks = message.Blocks
		}
		if blocksCarryImages(blocks) {
			return true
		}
	}
	return false
}

func blocksCarryImages(blocks []content.Block) bool {
	for _, block := range blocks {
		switch block := block.(type) {
		case *content.ImageBlock:
			return true
		case *content.ToolResultBlock:
			if blocksCarryImages(block.Content) {
				return true
			}
		}
	}
	return false
}
