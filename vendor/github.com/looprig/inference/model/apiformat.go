package model

// APIFormat names the wire dialect a model endpoint speaks. It is an OPEN label: inference
// carries built-in constant names for the bundled codecs/routes, but the type is deliberately
// not a validation gate. Unknown values are allowed — a caller supplying explicit
// request/response/stream decoders and a Router can name any dialect it likes. Fail-closed
// validation of known provider/format pairs belongs in the llm module or consumer code, never
// here. Note there is no APIFormat.Valid(): the absence of a fail-closed gate is intentional.
type APIFormat string

const (
	APIFormatOpenAI    APIFormat = "openai"
	APIFormatAnthropic APIFormat = "anthropic"
	APIFormatGemini    APIFormat = "gemini"

	// APIFormatOpenAIResponses names the OpenAI Responses API dialect
	// (POST /v1/responses), distinct from APIFormatOpenAI which names OpenAI
	// Chat Completions.
	APIFormatOpenAIResponses APIFormat = "openai-responses"

	// APIFormatBedrockConverse names Amazon Bedrock's model-neutral Converse
	// and ConverseStream APIs.
	APIFormatBedrockConverse APIFormat = "bedrock-converse"
)
