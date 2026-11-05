package web

import (
	"encoding/json"
	"orchids-api/internal/chatwire"
	"strings"
)

func parseVideoInputReferences(value interface{}) []string {
	var out []string
	var walk func(interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case nil:
			return
		case string:
			if s := strings.TrimSpace(x); s != "" {
				out = append(out, s)
			}
		case []interface{}:
			for _, item := range x {
				walk(item)
			}
		case map[string]interface{}:
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
		AspectRatio    interface{} `json:"aspect_ratio"`
		VideoLength    interface{} `json:"video_length"`
		Seconds        interface{} `json:"seconds"`
		ResolutionName interface{} `json:"resolution_name"`
		Preset         interface{} `json:"preset"`
		Size           interface{} `json:"size"`
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
		N              interface{} `json:"n"`
		Size           interface{} `json:"size"`
		ResponseFormat interface{} `json:"response_format"`
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
		Model               string                   `json:"model"`
		Messages            []ChatMessage            `json:"messages"`
		Stream              interface{}              `json:"stream"`
		Thinking            *string                  `json:"thinking,omitempty"`
		ReasoningEffort     *string                  `json:"reasoning_effort,omitempty"`
		ReasoningSummary    *string                  `json:"reasoning_summary,omitempty"`
		Temperature         interface{}              `json:"temperature,omitempty"`
		TopP                interface{}              `json:"top_p,omitempty"`
		MaxTokens           interface{}              `json:"max_tokens,omitempty"`
		MaxCompletionTokens interface{}              `json:"max_completion_tokens,omitempty"`
		ResponseFormat      map[string]interface{}   `json:"response_format,omitempty"`
		User                interface{}              `json:"user,omitempty"`
		SafetyIdentifier    interface{}              `json:"safety_identifier,omitempty"`
		ResponseText        map[string]interface{}   `json:"text,omitempty"`
		ResponsesTools      []map[string]interface{} `json:"x_responses_tools,omitempty"`
		Include             []string                 `json:"include,omitempty"`
		VideoConfig         *VideoConfig             `json:"video_config,omitempty"`
		ImageConfig         *ImageConfig             `json:"image_config,omitempty"`
		Tools               []ToolDef                `json:"tools,omitempty"`
		ToolChoice          interface{}              `json:"tool_choice,omitempty"`
		ParallelToolCalls   interface{}              `json:"parallel_tool_calls,omitempty"`
		Stop                interface{}              `json:"stop,omitempty"`
		PromptCacheKey      string                   `json:"prompt_cache_key,omitempty"`
		MCPServers          []map[string]interface{} `json:"mcp_servers,omitempty"`
		OutputConfig        map[string]interface{}   `json:"output_config,omitempty"`
		ThinkingConfig      map[string]interface{}   `json:"thinking_config,omitempty"`
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
	r.ResponsesTools = append([]map[string]interface{}(nil), raw.ResponsesTools...)
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
		Model          interface{}     `json:"model"`
		Prompt         interface{}     `json:"prompt"`
		N              interface{}     `json:"n"`
		PartialImages  interface{}     `json:"partial_images"`
		Size           interface{}     `json:"size"`
		AspectRatio    interface{}     `json:"aspect_ratio"`
		Resolution     interface{}     `json:"resolution"`
		Quality        interface{}     `json:"quality"`
		Stream         interface{}     `json:"stream"`
		NSFW           interface{}     `json:"nsfw"`
		ResponseFormat interface{}     `json:"response_format"`
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
		Model           interface{} `json:"model"`
		Prompt          interface{} `json:"prompt"`
		Seconds         interface{} `json:"seconds"`
		Duration        interface{} `json:"duration"`
		VideoLength     interface{} `json:"video_length"`
		Size            interface{} `json:"size"`
		AspectRatio     interface{} `json:"aspect_ratio"`
		ResolutionName  interface{} `json:"resolution_name"`
		Resolution      interface{} `json:"resolution"`
		Preset          interface{} `json:"preset"`
		InputReference  interface{} `json:"input_reference"`
		InputReferences interface{} `json:"input_references"`
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
