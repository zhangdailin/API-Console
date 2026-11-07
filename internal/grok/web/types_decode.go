package web

import (
	"encoding/json"
	"orchids-api/internal/chatwire"
	"strings"
)

func parseVideoInputReferences(value any) []string {
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case nil:
			return
		case string:
			if s := strings.TrimSpace(x); s != "" {
				out = append(out, s)
			}
		case []any:
			for _, item := range x {
				walk(item)
			}
		case map[string]any:
			for _, key := range []string{"image_url", "url", "data"} {
				if s := chatwire.ParseLooseStringAny(x[key]); s != "" {
					out = append(out, s)
					return
				}
			}
		}
	}
	walk(value)
	return uniqueStrings(out)
}

func (v *VideoConfig) UnmarshalJSON(data []byte) error {
	type rawVideoConfig struct {
		AspectRatio    any `json:"aspect_ratio"`
		VideoLength    any `json:"video_length"`
		Seconds        any `json:"seconds"`
		ResolutionName any `json:"resolution_name"`
		Preset         any `json:"preset"`
		Size           any `json:"size"`
	}
	var raw rawVideoConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	videoLength, err := chatwire.ParseLooseIntAny(raw.VideoLength)
	if err != nil {
		return err
	}
	if videoLength == 0 {
		videoLength, err = chatwire.ParseLooseIntAny(raw.Seconds)
		if err != nil {
			return err
		}
	}
	v.AspectRatio = chatwire.ParseLooseStringAny(raw.AspectRatio)
	v.VideoLength = videoLength
	v.ResolutionName = chatwire.ParseLooseStringAny(raw.ResolutionName)
	v.Preset = chatwire.ParseLooseStringAny(raw.Preset)
	v.Size = chatwire.ParseLooseStringAny(raw.Size)
	return nil
}

func (c *ImageConfig) UnmarshalJSON(data []byte) error {
	type rawImageConfig struct {
		N              any `json:"n"`
		Size           any `json:"size"`
		ResponseFormat any `json:"response_format"`
	}
	var raw rawImageConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	n, err := chatwire.ParseLooseIntAny(raw.N)
	if err != nil {
		return err
	}
	c.N = n
	c.Size = chatwire.ParseLooseStringAny(raw.Size)
	c.ResponseFormat = chatwire.ParseLooseStringAny(raw.ResponseFormat)
	return nil
}

func (r *ChatCompletionsRequest) UnmarshalJSON(data []byte) error {
	type rawChatRequest struct {
		Model               string           `json:"model"`
		Messages            []ChatMessage    `json:"messages"`
		Stream              any              `json:"stream"`
		Thinking            *string          `json:"thinking,omitempty"`
		ReasoningEffort     *string          `json:"reasoning_effort,omitempty"`
		ReasoningSummary    *string          `json:"reasoning_summary,omitempty"`
		Temperature         any              `json:"temperature,omitempty"`
		TopP                any              `json:"top_p,omitempty"`
		MaxTokens           any              `json:"max_tokens,omitempty"`
		MaxCompletionTokens any              `json:"max_completion_tokens,omitempty"`
		ResponseFormat      map[string]any   `json:"response_format,omitempty"`
		User                any              `json:"user,omitempty"`
		SafetyIdentifier    any              `json:"safety_identifier,omitempty"`
		ResponseText        map[string]any   `json:"text,omitempty"`
		ResponsesTools      []map[string]any `json:"x_responses_tools,omitempty"`
		Include             []string         `json:"include,omitempty"`
		VideoConfig         *VideoConfig     `json:"video_config,omitempty"`
		ImageConfig         *ImageConfig     `json:"image_config,omitempty"`
		Tools               []ToolDef        `json:"tools,omitempty"`
		ToolChoice          any              `json:"tool_choice,omitempty"`
		ParallelToolCalls   any              `json:"parallel_tool_calls,omitempty"`
		Stop                any              `json:"stop,omitempty"`
		PromptCacheKey      string           `json:"prompt_cache_key,omitempty"`
		MCPServers          []map[string]any `json:"mcp_servers,omitempty"`
		OutputConfig        map[string]any   `json:"output_config,omitempty"`
		ThinkingConfig      map[string]any   `json:"thinking_config,omitempty"`
	}

	var raw rawChatRequest
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var rawMap map[string]json.RawMessage
	_ = json.Unmarshal(data, &rawMap)
	streamRaw, streamProvided := rawMap["stream"]
	if streamProvided {
		s := strings.TrimSpace(string(streamRaw))
		if s == "" || strings.EqualFold(s, "null") {
			streamProvided = false
		}
	}
	stream, err := chatwire.ParseLooseBoolAny(raw.Stream)
	if err != nil {
		return err
	}
	temp, err := chatwire.ParseLooseFloatAny(raw.Temperature)
	if err != nil {
		return err
	}
	topP, err := chatwire.ParseLooseFloatAny(raw.TopP)
	if err != nil {
		return err
	}
	maxTokens, err := chatwire.ParseLooseIntAny(raw.MaxTokens)
	if err != nil {
		return err
	}
	maxCompletionTokens, err := chatwire.ParseLooseIntAny(raw.MaxCompletionTokens)
	if err != nil {
		return err
	}
	parallelToolCalls, err := chatwire.ParseLooseBoolAnyForField(raw.ParallelToolCalls, "parallel_tool_calls")
	if err != nil {
		return err
	}

	r.Model = raw.Model
	r.Messages = raw.Messages
	r.Stream = stream
	r.StreamProvided = streamProvided
	r.Thinking = raw.Thinking
	r.ReasoningEffort = raw.ReasoningEffort
	r.ReasoningSummary = raw.ReasoningSummary
	r.Temperature = temp
	r.TopP = topP
	if _, ok := rawMap["max_tokens"]; ok {
		r.MaxTokens = &maxTokens
	}
	if _, ok := rawMap["max_completion_tokens"]; ok {
		r.MaxCompletionTokens = &maxCompletionTokens
		// max_tokens is deprecated in favour of max_completion_tokens, so when a
		// caller sends both the newer field decides the output budget.
		r.MaxTokens = &maxCompletionTokens
	}
	r.ResponseFormat = raw.ResponseFormat
	r.SafetyIdentifier = firstNonEmpty(chatwire.ParseLooseStringAny(raw.SafetyIdentifier), chatwire.ParseLooseStringAny(raw.User))
	r.ResponseText = raw.ResponseText
	r.ResponsesTools = append([]map[string]any(nil), raw.ResponsesTools...)
	r.Include = append([]string(nil), raw.Include...)
	r.VideoConfig = raw.VideoConfig
	r.ImageConfig = raw.ImageConfig
	r.Tools = raw.Tools
	r.ToolChoice = raw.ToolChoice
	if _, ok := rawMap["parallel_tool_calls"]; ok {
		r.ParallelToolCalls = &parallelToolCalls
	}
	r.Stop, err = chatwire.ParseStringList(raw.Stop, "stop")
	if err != nil {
		return err
	}
	r.PromptCacheKey = strings.TrimSpace(raw.PromptCacheKey)
	r.MCPServers = raw.MCPServers
	r.OutputConfig = raw.OutputConfig
	r.ThinkingConfig = raw.ThinkingConfig
	return nil
}

func (r *ImagesGenerationsRequest) UnmarshalJSON(data []byte) error {
	type rawImagesGenerationsRequest struct {
		Model          any             `json:"model"`
		Prompt         any             `json:"prompt"`
		N              any             `json:"n"`
		PartialImages  any             `json:"partial_images"`
		Size           any             `json:"size"`
		AspectRatio    any             `json:"aspect_ratio"`
		Resolution     any             `json:"resolution"`
		Quality        any             `json:"quality"`
		Stream         any             `json:"stream"`
		NSFW           any             `json:"nsfw"`
		ResponseFormat any             `json:"response_format"`
		StorageOptions json.RawMessage `json:"storage_options"`
	}
	var raw rawImagesGenerationsRequest
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	n, err := chatwire.ParseLooseIntAny(raw.N)
	if err != nil {
		return err
	}
	if raw.N == nil {
		// An omitted (or null) n defaults to 1, matching OpenAI and grok2api.
		// Only an explicit out-of-range value is an error, and every caller
		// downstream relies on n >= 1.
		n = 1
	}
	var partialImages *int
	if raw.PartialImages != nil {
		value, err := chatwire.ParseLooseIntAny(raw.PartialImages)
		if err != nil {
			return err
		}
		partialImages = &value
	}
	stream, err := chatwire.ParseLooseBoolAny(raw.Stream)
	if err != nil {
		return err
	}
	var nsfw *bool
	if raw.NSFW != nil {
		nsfwVal, err := chatwire.ParseLooseBoolAnyForField(raw.NSFW, "nsfw")
		if err != nil {
			return err
		}
		nsfw = &nsfwVal
	}
	r.Model = chatwire.ParseLooseStringAny(raw.Model)
	r.Prompt = chatwire.ParseLooseStringAny(raw.Prompt)
	r.N = n
	r.PartialImages = partialImages
	r.Size = chatwire.ParseLooseStringAny(raw.Size)
	r.AspectRatio = chatwire.ParseLooseStringAny(raw.AspectRatio)
	r.Resolution = chatwire.ParseLooseStringAny(raw.Resolution)
	r.Quality = chatwire.ParseLooseStringAny(raw.Quality)
	r.Stream = stream
	r.NSFW = nsfw
	r.ResponseFormat = chatwire.ParseLooseStringAny(raw.ResponseFormat)
	r.StorageOptions = append(r.StorageOptions[:0], raw.StorageOptions...)
	return nil
}

func (r *VideosRequest) UnmarshalJSON(data []byte) error {
	type rawVideosRequest struct {
		Model           any `json:"model"`
		Prompt          any `json:"prompt"`
		Seconds         any `json:"seconds"`
		Duration        any `json:"duration"`
		VideoLength     any `json:"video_length"`
		Size            any `json:"size"`
		AspectRatio     any `json:"aspect_ratio"`
		ResolutionName  any `json:"resolution_name"`
		Resolution      any `json:"resolution"`
		Preset          any `json:"preset"`
		InputReference  any `json:"input_reference"`
		InputReferences any `json:"input_references"`
	}
	var raw rawVideosRequest
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	seconds, err := chatwire.ParseLooseIntAny(raw.Seconds)
	if err != nil {
		return err
	}
	if seconds == 0 {
		seconds, err = chatwire.ParseLooseIntAny(raw.Duration)
		if err != nil {
			return err
		}
	}
	if seconds == 0 {
		seconds, err = chatwire.ParseLooseIntAny(raw.VideoLength)
		if err != nil {
			return err
		}
	}
	r.Model = chatwire.ParseLooseStringAny(raw.Model)
	r.Prompt = chatwire.ParseLooseStringAny(raw.Prompt)
	r.Seconds = seconds
	r.Size = chatwire.ParseLooseStringAny(raw.Size)
	if r.Size == "" {
		r.Size = chatwire.ParseLooseStringAny(raw.AspectRatio)
	}
	r.ResolutionName = chatwire.ParseLooseStringAny(raw.ResolutionName)
	if r.ResolutionName == "" {
		r.ResolutionName = chatwire.ParseLooseStringAny(raw.Resolution)
	}
	r.Preset = chatwire.ParseLooseStringAny(raw.Preset)
	r.InputReferences = parseVideoInputReferences(raw.InputReferences)
	if len(r.InputReferences) == 0 {
		r.InputReferences = parseVideoInputReferences(raw.InputReference)
	}
	return nil
}
