package grok

import (
	"fmt"
	"github.com/goccy/go-json"
	"strconv"
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

func parseLooseBoolAnyForField(value interface{}, field string) (bool, error) {
	if strings.TrimSpace(field) == "" {
		field = "value"
	}
	errText := field + " must be a boolean"
	switch v := value.(type) {
	case nil:
		return false, nil
	case bool:
		return v, nil
	case string:
		raw := strings.TrimSpace(v)
		if raw == "" {
			return false, nil
		}
		switch strings.ToLower(raw) {
		case "1", "true", "yes", "y", "on":
			return true, nil
		case "0", "false", "no", "n", "off":
			return false, nil
		default:
			return false, fmt.Errorf("%s", errText)
		}
	case float64:
		if v == 1 {
			return true, nil
		}
		if v == 0 {
			return false, nil
		}
		return false, fmt.Errorf("%s", errText)
	default:
		return false, fmt.Errorf("%s", errText)
	}
}

func parseLooseBoolAny(value interface{}) (bool, error) {
	return parseLooseBoolAnyForField(value, "stream")
}

func parseLooseIntAny(value interface{}) (int, error) {
	switch v := value.(type) {
	case nil:
		return 0, nil
	case int:
		return v, nil
	case int32:
		return int(v), nil
	case int64:
		return int(v), nil
	case float64:
		return int(v), nil
	case string:
		raw := strings.TrimSpace(v)
		if raw == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return 0, err
		}
		return n, nil
	default:
		return 0, fmt.Errorf("invalid integer value")
	}
}

func parseLooseFloatAny(value interface{}) (*float64, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case float64:
		out := v
		return &out, nil
	case int:
		out := float64(v)
		return &out, nil
	case int32:
		out := float64(v)
		return &out, nil
	case int64:
		out := float64(v)
		return &out, nil
	case string:
		raw := strings.TrimSpace(v)
		if raw == "" {
			return nil, nil
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, err
		}
		return &n, nil
	default:
		return nil, fmt.Errorf("invalid float value")
	}
}

func parseLooseStringAny(value interface{}) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
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
	stream, err := parseLooseBoolAny(raw.Stream)
	if err != nil {
		return err
	}
	temp, err := parseLooseFloatAny(raw.Temperature)
	if err != nil {
		return err
	}
	topP, err := parseLooseFloatAny(raw.TopP)
	if err != nil {
		return err
	}
	maxTokens, err := parseLooseIntAny(raw.MaxTokens)
	if err != nil {
		return err
	}
	maxCompletionTokens, err := parseLooseIntAny(raw.MaxCompletionTokens)
	if err != nil {
		return err
	}
	parallelToolCalls, err := parseLooseBoolAnyForField(raw.ParallelToolCalls, "parallel_tool_calls")
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
	r.SafetyIdentifier = firstNonEmpty(parseLooseStringAny(raw.SafetyIdentifier), parseLooseStringAny(raw.User))
	r.ResponseText = raw.ResponseText
	r.ResponsesTools = append([]map[string]interface{}(nil), raw.ResponsesTools...)
	r.Include = append([]string(nil), raw.Include...)
	r.Tools = raw.Tools
	r.ToolChoice = raw.ToolChoice
	if _, ok := rawMap["parallel_tool_calls"]; ok {
		r.ParallelToolCalls = &parallelToolCalls
	}
	r.Stop, err = parseStringList(raw.Stop, "stop")
	if err != nil {
		return err
	}
	r.PromptCacheKey = strings.TrimSpace(raw.PromptCacheKey)
	r.MCPServers = raw.MCPServers
	r.OutputConfig = raw.OutputConfig
	r.ThinkingConfig = raw.ThinkingConfig
	return nil
}

func parseStringList(value interface{}, field string) ([]string, error) {
	switch item := value.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{item}, nil
	case []interface{}:
		out := make([]string, 0, len(item))
		for _, raw := range item {
			text, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a string or array of strings", field)
			}
			out = append(out, text)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be a string or array of strings", field)
	}
}

type RateLimitInfo struct {
	Limit        int64
	HasLimit     bool
	Remaining    int64
	HasRemaining bool
	ResetAt      time.Time
	Unit         string
}

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
	return nil
}

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
