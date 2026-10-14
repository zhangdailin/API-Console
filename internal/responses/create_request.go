package responses

import "encoding/json"

// CreateRequest is the OpenAI Responses create request as a client sends it.
//
// It is decoded in two passes (see UnmarshalJSON): the members whose wire shape
// is already the one the gateway consumes arrive through the embedded struct,
// decoded exactly as declared, so a malformed value is a request error instead
// of a silently dropped field. The scalar members accept several JSON shapes
// ("true", "1", a numeric string), so interface{} members shadow the typed ones
// and are parsed loosely afterwards.
type CreateRequest struct {
	Model              string           `json:"model"`
	Input              any              `json:"input"`
	Instructions       string           `json:"instructions,omitempty"`
	Stream             bool             `json:"stream,omitempty"`
	StreamProvided     bool             `json:"-"`
	Reasoning          map[string]any   `json:"reasoning,omitempty"`
	Temperature        *float64         `json:"temperature,omitempty"`
	TopP               *float64         `json:"top_p,omitempty"`
	MaxOutputTokens    *int             `json:"max_output_tokens,omitempty"`
	Tools              []map[string]any `json:"tools,omitempty"`
	ToolChoice         any              `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool            `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID string           `json:"previous_response_id,omitempty"`
	Store              *bool            `json:"store,omitempty"`
	Metadata           map[string]any   `json:"metadata,omitempty"`
	Truncation         string           `json:"truncation,omitempty"`
	Include            []string         `json:"include,omitempty"`
	Background         *bool            `json:"background,omitempty"`
	PromptCacheKey     string           `json:"prompt_cache_key,omitempty"`
	// Text carries the Responses text controls, whose only member is the output
	// format (`text.format`). The chat-only channels express the same thing as
	// `response_format`, so the bridge passes the object through unchanged and
	// lets the chat layer decide how much of it the upstream honors.
	Text map[string]any `json:"text,omitempty"`
	// ResponseFormat accepts the chat-shaped field as well. Clients that were
	// written against the chat API and later migrated to Responses keep sending
	// it, and silently dropping it used to turn a structured-output request into
	// free-form prose.
	ResponseFormat map[string]any `json:"response_format,omitempty"`
}

func (r *CreateRequest) UnmarshalJSON(data []byte) error {
	type plainCreateRequest CreateRequest
	type rawCreateRequest struct {
		plainCreateRequest
		Model              any `json:"model"`
		Instructions       any `json:"instructions,omitempty"`
		Stream             any `json:"stream,omitempty"`
		Temperature        any `json:"temperature,omitempty"`
		TopP               any `json:"top_p,omitempty"`
		MaxOutputTokens    any `json:"max_output_tokens,omitempty"`
		ParallelToolCalls  any `json:"parallel_tool_calls,omitempty"`
		PreviousResponseID any `json:"previous_response_id,omitempty"`
		Store              any `json:"store,omitempty"`
		Truncation         any `json:"truncation,omitempty"`
		Background         any `json:"background,omitempty"`
		PromptCacheKey     any `json:"prompt_cache_key,omitempty"`
	}

	var raw rawCreateRequest
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	stream, err := ParseLooseBoolAny(raw.Stream)
	if err != nil {
		return err
	}
	temp, err := ParseLooseFloatAny(raw.Temperature)
	if err != nil {
		return err
	}
	topP, err := ParseLooseFloatAny(raw.TopP)
	if err != nil {
		return err
	}
	maxOutputTokens, err := ParseLooseIntAny(raw.MaxOutputTokens)
	if err != nil {
		return err
	}
	var rawMap map[string]json.RawMessage
	_ = json.Unmarshal(data, &rawMap)
	_, streamProvided := rawMap["stream"]
	var parallel *bool
	if _, ok := rawMap["parallel_tool_calls"]; ok {
		v, err := ParseLooseBoolAnyForField(raw.ParallelToolCalls, "parallel_tool_calls")
		if err != nil {
			return err
		}
		parallel = &v
	}
	var store *bool
	if _, ok := rawMap["store"]; ok {
		v, err := ParseLooseBoolAnyForField(raw.Store, "store")
		if err != nil {
			return err
		}
		store = &v
	}
	var background *bool
	if _, ok := rawMap["background"]; ok {
		v, err := ParseLooseBoolAnyForField(raw.Background, "background")
		if err != nil {
			return err
		}
		background = &v
	}
	var maxOutput *int
	if _, ok := rawMap["max_output_tokens"]; ok {
		maxOutput = &maxOutputTokens
	}

	*r = CreateRequest(raw.plainCreateRequest)
	r.Model = ParseLooseStringAny(raw.Model)
	r.Instructions = ParseLooseStringAny(raw.Instructions)
	r.Stream = stream
	r.StreamProvided = streamProvided
	r.Temperature = temp
	r.TopP = topP
	r.MaxOutputTokens = maxOutput
	r.ParallelToolCalls = parallel
	r.PreviousResponseID = ParseLooseStringAny(raw.PreviousResponseID)
	r.Store = store
	r.Truncation = ParseLooseStringAny(raw.Truncation)
	r.Background = background
	r.PromptCacheKey = ParseLooseStringAny(raw.PromptCacheKey)
	return nil
}
