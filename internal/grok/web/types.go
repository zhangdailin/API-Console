package web

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/store"
)

type chatSourceOperationKey struct{}

// ChatCompletionsRequest is the shared Chat Completions request plus the
// media options only the Web plane accepts. Field decoding (loose JSON, the
// max_completion_tokens precedence, stream presence) is chatwire's, so a fix
// there reaches this plane too.
type ChatCompletionsRequest struct {
	chatwire.Request
	VideoConfig *VideoConfig `json:"video_config,omitempty"`
	ImageConfig *ImageConfig `json:"image_config,omitempty"`
	account     *store.Account
}

// The message and tool shapes are the shared Chat Completions wire types.
type (
	ChatMessage = chatwire.Message
	ToolDef     = chatwire.ToolDef
	ToolCall    = chatwire.ToolCall
)

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
	store.StoredVideoJob
	Error         map[string]any
	PublicBaseURL string
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

type RateLimitInfo = chatwire.RateLimitInfo

const (
	maxToolDefinitions      = 128
	maxToolDescriptionBytes = 16 << 10
)

var grokToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// validateWebToolLimits applies prompt emulation limits after wire validation.
func validateWebToolLimits(tools []ToolDef) error {
	if len(tools) > maxToolDefinitions {
		return fmt.Errorf("tools must contain at most %d items on Web", maxToolDefinitions)
	}
	for i, tool := range tools {
		if tool.Function == nil {
			// Hosted tools have no function for Web to render into the prompt.
			continue
		}
		name, _ := tool.Function["name"].(string)
		if !grokToolNamePattern.MatchString(strings.TrimSpace(name)) {
			return fmt.Errorf("tools.%d.function.name must match [A-Za-z0-9_-]{1,64} on Web", i)
		}
		if description := strings.TrimSpace(chatwire.ParseLooseStringAny(tool.Function["description"])); len(description) > maxToolDescriptionBytes {
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
	if err := normalizeChatDocuments(r.Messages); err != nil {
		return err
	}
	chatwire.DropBlankUserTextParts(r.Messages)
	if err := chatwire.ValidateMessages(r.Messages); err != nil {
		return err
	}
	if err := chatwire.ValidateStrictToolDefinitions(r.Tools); err != nil {
		return err
	}
	if err := r.ValidateToolChoice(); err != nil {
		return err
	}
	if r.ImageConfig != nil {
		cfg := r.ImageConfig
		cfg.Normalize()
		if cfg.N > 10 {
			return fmt.Errorf("image_config.n must be between 1 and 10")
		}
		modelID := normalizeModelID(r.Model)
		if modelID == "grok-imagine-image-lite" && cfg.N > 4 {
			return fmt.Errorf("image_config.n must be between 1 and 4 for grok-imagine-image-lite")
		}
		normalizeSize := normalizeImageSize
		if modelID == "grok-imagine-image-edit" {
			if cfg.N > 2 {
				return fmt.Errorf("image_config.n must be between 1 and 2 for image edit")
			}
			normalizeSize = normalizeImageEditSize
		}
		size, err := normalizeSize(cfg.Size)
		if err != nil {
			return err
		}
		cfg.Size = size
		if r.Stream && cfg.N > 2 {
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
