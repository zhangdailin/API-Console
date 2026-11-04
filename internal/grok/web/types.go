package web

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"orchids-api/internal/store"
)

type chatSourceOperationKey struct{}

type ChatCompletionsRequest struct {
	Model               string                   `json:"model"`
	Messages            []ChatMessage            `json:"messages"`
	Stream              bool                     `json:"stream"`
	StreamProvided      bool                     `json:"-"`
	Thinking            *string                  `json:"thinking,omitempty"`
	ReasoningEffort     *string                  `json:"reasoning_effort,omitempty"`
	ReasoningSummary    *string                  `json:"reasoning_summary,omitempty"`
	Temperature         *float64                 `json:"temperature,omitempty"`
	TopP                *float64                 `json:"top_p,omitempty"`
	MaxTokens           *int                     `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                     `json:"max_completion_tokens,omitempty"`
	ResponseFormat      map[string]interface{}   `json:"response_format,omitempty"`
	SafetyIdentifier    string                   `json:"safety_identifier,omitempty"`
	ResponseText        map[string]interface{}   `json:"text,omitempty"`
	ResponsesTools      []map[string]interface{} `json:"x_responses_tools,omitempty"`
	ResponsesInput      []interface{}            `json:"x_responses_input,omitempty"`
	Include             []string                 `json:"include,omitempty"`
	VideoConfig         *VideoConfig             `json:"video_config,omitempty"`
	ImageConfig         *ImageConfig             `json:"image_config,omitempty"`
	Tools               []ToolDef                `json:"tools,omitempty"`
	ToolChoice          interface{}              `json:"tool_choice,omitempty"`
	// WebSearchOptions is the OpenAI-style switch for the hosted search tool.
	// It is lowered to a native web_search tool on the Responses planes.
	WebSearchOptions  map[string]interface{}   `json:"web_search_options,omitempty"`
	ParallelToolCalls *bool                    `json:"parallel_tool_calls,omitempty"`
	Stop              []string                 `json:"stop,omitempty"`
	PromptCacheKey    string                   `json:"prompt_cache_key,omitempty"`
	MCPServers        []map[string]interface{} `json:"mcp_servers,omitempty"`
	Metadata          map[string]interface{}   `json:"metadata,omitempty"`
	ServiceTier       string                   `json:"service_tier,omitempty"`
	OutputConfig      map[string]interface{}   `json:"output_config,omitempty"`
	ThinkingConfig    map[string]interface{}   `json:"thinking_config,omitempty"`
	ReasoningReplay   bool                     `json:"-"`
	startedAt         time.Time
	sourceOperation   string
	account           *store.Account
}

type ChatMessage struct {
	Role                      string      `json:"role"`
	Content                   interface{} `json:"content"`
	ToolCalls                 []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID                string      `json:"tool_call_id,omitempty"`
	Name                      string      `json:"name,omitempty"`
	ReasoningContent          string      `json:"reasoning_content,omitempty"`
	ReasoningEncryptedContent string      `json:"reasoning_encrypted_content,omitempty"`
}

type ToolDef struct {
	Type     string                 `json:"type"`
	Function map[string]interface{} `json:"function,omitempty"`
	// Raw keeps every field of the declaration. Hosted tool types (web_search,
	// x_search, …) carry their own parameters outside `function`, and dropping
	// them would silently disable the feature the caller asked for.
	Raw map[string]interface{} `json:"-"`
}

// UnmarshalJSON keeps the whole declaration so native (non-function) tools can
// be forwarded with their own fields intact.
func (t *ToolDef) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	t.Raw = raw
	t.Type = strings.TrimSpace(parseLooseStringAny(raw["type"]))
	if fn, ok := raw["function"].(map[string]interface{}); ok {
		t.Function = fn
	}
	return nil
}

type ToolCall struct {
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function map[string]interface{} `json:"function,omitempty"`
}

type VideoConfig struct {
	AspectRatio    string `json:"aspect_ratio"`
	VideoLength    int    `json:"video_length"`
	ResolutionName string `json:"resolution_name"`
	Preset         string `json:"preset"`
	Size           string `json:"size,omitempty"`
}

type VideosRequest struct {
	Model           string `json:"model"`
	Prompt          string `json:"prompt"`
	Seconds         int    `json:"seconds"`
	Size            string `json:"size"`
	ResolutionName  string `json:"resolution_name"`
	Preset          string `json:"preset"`
	InputReferences []string
	// grok2api-compatible aliases. The same options are spelled differently by
	// the two public surfaces, and dropping the alias silently fell back to the
	// defaults (8 seconds, 16:9) while reporting success.
	Duration        json.RawMessage `json:"duration,omitempty"`
	AspectRatio     string          `json:"aspect_ratio,omitempty"`
	Resolution      string          `json:"resolution,omitempty"`
	User            string          `json:"user,omitempty"`
	Image           string          `json:"image,omitempty"`
	ReferenceImages []string        `json:"reference_images,omitempty"`
	ReferenceAudios []string        `json:"reference_audios,omitempty"`
	Video           string          `json:"video,omitempty"`
}

type videoJob struct {
	ID                string
	Model             string
	Prompt            string
	Seconds           int
	Size              string
	Quality           string
	CreatedAt         int64
	Status            string
	Progress          int
	CompletedAt       int64
	Error             map[string]interface{}
	VideoURL          string
	ContentPath       string
	RemixedFromID     string
	InputReferences   []string
	Operation         string
	StandardAPI       bool
	OwnerHash         string
	AccountID         int64
	Provider          string
	UpstreamRequestID string
	PublicBaseURL     string
	BuildFallback     bool
}

type ImageConfig struct {
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
	// grok2api's image_config also carries the ratio and the resolution tier.
	// Without them a caller could not ask for either, and a request that got its
	// options through the top-level fields behaved differently depending on which
	// surface it used.
	AspectRatio string `json:"aspect_ratio,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
}

type ImagesGenerationsRequest struct {
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	N              int             `json:"n"`
	PartialImages  *int            `json:"partial_images,omitempty"`
	Size           string          `json:"size"`
	AspectRatio    string          `json:"aspect_ratio"`
	Resolution     string          `json:"resolution"`
	Quality        string          `json:"quality"`
	Stream         bool            `json:"stream"`
	NSFW           *bool           `json:"nsfw,omitempty"`
	ResponseFormat string          `json:"response_format"`
	StorageOptions json.RawMessage `json:"storage_options,omitempty"`
}

type RateLimitInfo struct {
	Limit        int64
	HasLimit     bool
	Remaining    int64
	HasRemaining bool
	ResetAt      time.Time
	Unit         string
	// WindowSeconds is the upstream windowSizeSeconds, when present.
	WindowSeconds int64
}

const (
	maxToolDefinitions      = 128
	maxToolDescriptionBytes = 16 << 10
)

var grokToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// nativeToolTypes are the hosted (server-side) tools an OpenAI-compatible
// client may declare in `tools`. They have no `function` object; they are
// forwarded to the upstream Responses plane as native tools.
var nativeToolTypes = map[string]string{
	"web_search":                    "web_search",
	"web_search_preview":            "web_search",
	"web_search_preview_2025_03_11": "web_search",
	"web_search_2025_08_26":         "web_search",
	"x_search":                      "x_search",
}

func validateToolDefinitions(tools []ToolDef) error {
	seen := make(map[string]struct{}, len(tools))
	for i, tool := range tools {
		if normalized, native := nativeToolTypes[strings.ToLower(strings.TrimSpace(tool.Type))]; native {
			key := normalized
			if _, exists := seen[key]; exists {
				return fmt.Errorf("tools.%d.type duplicates %q", i, normalized)
			}
			seen[key] = struct{}{}
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(tool.Type), "function") {
			return fmt.Errorf("tools.%d.type must be function, web_search or x_search", i)
		}
		if tool.Function == nil {
			return fmt.Errorf("tools.%d.function is required", i)
		}
		name, ok := tool.Function["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			return fmt.Errorf("tools.%d.function.name must be a non-empty string", i)
		}
		key := strings.TrimSpace(name)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("tools.%d.function.name duplicates %q", i, name)
		}
		seen[key] = struct{}{}
		if parameters, ok := tool.Function["parameters"]; ok && parameters != nil {
			raw, err := json.Marshal(parameters)
			if err != nil {
				return fmt.Errorf("tools.%d.function.parameters must be valid JSON", i)
			}
			var decoded interface{}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return fmt.Errorf("tools.%d.function.parameters must be valid JSON", i)
			}
		}
	}
	return nil
}

// Web emulates tools in a prompt; its limits are not Build/Console contracts.
func validateWebToolDefinitions(tools []ToolDef) error {
	if err := validateToolDefinitions(tools); err != nil {
		return err
	}
	if len(tools) > maxToolDefinitions {
		return fmt.Errorf("tools must contain at most %d items on Web", maxToolDefinitions)
	}
	for i, tool := range tools {
		if tool.Function == nil {
			// A hosted tool (web_search / x_search) has no function object.
			// Web emulates tools in the prompt, so it cannot run one
			// server-side; the search it would have asked for is the same
			// upstream search the Web plane already enables per request.
			// Skipping keeps the turn valid — indexing the missing declaration
			// used to panic the whole request.
			continue
		}
		if !grokToolNamePattern.MatchString(strings.TrimSpace(tool.Function["name"].(string))) {
			return fmt.Errorf("tools.%d.function.name must match [A-Za-z0-9_-]{1,64} on Web", i)
		}
		if description := strings.TrimSpace(parseLooseStringAny(tool.Function["description"])); len(description) > maxToolDescriptionBytes {
			return fmt.Errorf("tools.%d.function.description must be at most %d bytes on Web", i, maxToolDescriptionBytes)
		}
	}
	return nil
}

func (r *ChatCompletionsRequest) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("model is required")
	}
	if len(r.Messages) == 0 {
		return fmt.Errorf("messages is required")
	}
	if err := validateChatMessages(r.Messages); err != nil {
		return err
	}
	if err := validateToolDefinitions(r.Tools); err != nil {
		return err
	}
	if r.ToolChoice != nil {
		switch v := r.ToolChoice.(type) {
		case string:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "auto", "none":
			case "required":
				if len(r.toolChoiceNameSet()) == 0 {
					return fmt.Errorf("tool_choice required needs at least one defined tool")
				}
			default:
				return fmt.Errorf("tool_choice must be auto, required, none, or a specific function object")
			}
		case map[string]interface{}:
			fn, _ := v["function"].(map[string]interface{})
			name, _ := fn["name"].(string)
			name = strings.TrimSpace(name)
			if strings.TrimSpace(fmt.Sprint(v["type"])) != "function" || name == "" {
				return fmt.Errorf("tool_choice object must have type=function and function.name")
			}
			// Hosted tools (web_search / x_search) live in ResponsesTools, not
			// in the function list, so both sets decide whether the name exists.
			if _, found := r.toolChoiceNameSet()[name]; !found {
				return fmt.Errorf("tool_choice.function.name must reference a defined tool")
			}
		default:
			return fmt.Errorf("tool_choice must be auto, required, none, or a specific function object")
		}
	}
	if r.ImageConfig != nil {
		r.ImageConfig.Normalize()
		if r.ImageConfig.N < 1 || r.ImageConfig.N > 10 {
			return fmt.Errorf("image_config.n must be between 1 and 10")
		}
		modelID := normalizeModelID(r.Model)
		if modelID == "grok-imagine-image-lite" && r.ImageConfig.N > 4 {
			return fmt.Errorf("image_config.n must be between 1 and 4 for grok-imagine-image-lite")
		}
		if modelID == "grok-imagine-image-edit" && r.ImageConfig.N > 2 {
			return fmt.Errorf("image_config.n must be between 1 and 2 for image edit")
		}
		if r.ImageConfig.ResponseFormat != "" {
			switch normalizeImageResponseFormat(r.ImageConfig.ResponseFormat) {
			case "b64_json", "url":
				// ok
			default:
				return fmt.Errorf("image_config.response_format must be one of b64_json, base64, url")
			}
			r.ImageConfig.ResponseFormat = normalizeImageResponseFormat(r.ImageConfig.ResponseFormat)
		}
		size, err := normalizeImageSize(r.ImageConfig.Size)
		if modelID == "grok-imagine-image-edit" {
			size, err = normalizeImageEditSize(r.ImageConfig.Size)
		}
		if err != nil {
			return err
		}
		r.ImageConfig.Size = size
		if r.Stream && r.ImageConfig.N > 2 {
			return fmt.Errorf("streaming is only supported when image_config.n=1 or n=2")
		}
	}
	return nil
}

func (r *ImagesGenerationsRequest) Normalize() {
	// No model default: grok2api rejects a request that names none, and silently
	// choosing one spent image quota on a request the caller never described.
	if r.N <= 0 {
		r.N = 1
	}
	if strings.TrimSpace(r.ResponseFormat) == "" {
		r.ResponseFormat = "url"
	}
}

func (c *ImageConfig) Normalize() {
	if c == nil {
		return
	}
	if c.N <= 0 {
		c.N = 1
	}
	if strings.TrimSpace(c.Size) == "" {
		c.Size = "1024x1024"
	}
	if strings.TrimSpace(c.ResponseFormat) == "" {
		c.ResponseFormat = "url"
	}
	c.ResponseFormat = normalizeImageResponseFormat(c.ResponseFormat)
}

func (v *VideoConfig) Normalize() {
	if v == nil {
		return
	}
	if v.VideoLength == 0 {
		v.VideoLength = 6
	}
	if strings.TrimSpace(v.Preset) == "" {
		v.Preset = "custom"
	}
}

// toolChoiceNameSet collects every tool name a tool_choice may refer to: the
// function tools plus the hosted tools (whose name is their type, e.g.
// web_search) that otherwise live only in ResponsesTools.
func (r *ChatCompletionsRequest) toolChoiceNameSet() map[string]struct{} {
	if r == nil {
		return nil
	}
	names := make(map[string]struct{}, len(r.Tools)+len(r.ResponsesTools))
	for _, tool := range r.Tools {
		if tool.Function == nil {
			if normalized, native := nativeToolTypes[strings.ToLower(strings.TrimSpace(tool.Type))]; native {
				names[normalized] = struct{}{}
			}
			continue
		}
		if name := strings.TrimSpace(fmt.Sprint(tool.Function["name"])); name != "" && name != "<nil>" {
			names[name] = struct{}{}
		}
	}
	for _, tool := range r.ResponsesTools {
		for _, key := range []string{"name", "type"} {
			if name := strings.TrimSpace(parseLooseStringAny(tool[key])); name != "" {
				names[name] = struct{}{}
			}
		}
	}
	return names
}
