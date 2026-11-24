package doubao

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"orchids-api/internal/httpclient"
	"orchids-api/internal/upstream"
)

const (
	ImageModel     = "doubao-image"
	ImageEditModel = "doubao-image-edit"
	VideoModel     = "doubao-video"
)

var (
	errVideoQuota = errors.New("Doubao daily video quota exhausted")
	errNoMedia    = errors.New("Doubao did not return a completed media result")
)

type generatedMedia struct {
	URL      string  `json:"url,omitempty"`
	Base64   string  `json:"b64_json,omitempty"`
	CoverURL string  `json:"cover_url,omitempty"`
	Width    int     `json:"width,omitempty"`
	Height   int     `json:"height,omitempty"`
	Duration float64 `json:"duration,omitempty"`
}

type mediaRequest struct {
	Model, Prompt, Ratio, Format string
	Count                        int
	References                   []promptBlock
}

func (c *Client) generateMedia(ctx context.Context, req mediaRequest) ([]generatedMedia, error) {
	video := req.Model == VideoModel
	if err := waitTurn(ctx, c.acc.ID, c.cfg); err != nil {
		return nil, err
	}
	jar := c.jar()
	if err := c.bindConversationIdentity(ctx, ""); err != nil {
		return nil, err
	}
	refs := make([]map[string]any, 0, len(req.References))
	for _, ref := range req.References {
		uri, err := c.uploadAttachment(ctx, jar, ref.name, ref.data, true)
		if err != nil {
			return nil, err
		}
		attachment := map[string]any{"type": "image", "key": uri}
		if !video {
			attachment["extra"] = map[string]string{"refer_types": "overall"}
		}
		refs = append(refs, attachment)
	}
	result, task, err := c.mediaCall(ctx, jar, buildMediaPayload(req, refs), video)
	if err != nil {
		return nil, err
	}
	if video && len(result) == 0 && task != "" {
		// Poll exactly the accepted task on the same account. A failure after
		// acceptance is never a reason to submit another video.
		result, _, err = c.mediaCall(ctx, jar, map[string]any{"task_id": task, "event_id": 0}, true)
		if err != nil {
			if errors.Is(err, errVideoQuota) {
				return nil, errNoMedia
			}
			return nil, err
		}
	}
	if len(result) < req.Count {
		return nil, errNoMedia
	}
	c.clearVerificationOnSuccess(ctx)
	return result[:req.Count], nil
}

func buildMediaPayload(req mediaRequest, refs []map[string]any) map[string]any {
	contentType, skill := 2009, 3
	if req.Model == VideoModel {
		contentType, skill = 2020, 17
	}
	content := map[string]string{"text": req.Prompt}
	if req.Ratio != "" {
		content["ratio"] = req.Ratio
	}
	body, _ := json.Marshal(content)
	return map[string]any{
		"messages": []any{map[string]any{"content": string(body), "content_type": contentType,
			"attachments": refs, "references": []any{},
			"skill": map[string]any{"skill_type": skill, "skill_type_no_default": skill,
				"skill_id": fmt.Sprint(skill), "skill_id_no_default": fmt.Sprint(skill)}}},
		"completion_option": map[string]any{
			"with_suggest": true, "need_create_conversation": true, "launch_stage": 1,
			"memory_type": 2, "message_from": 0, "action_bar_skill_id": skill,
			"is_regen": false, "is_replace": false, "is_delete": false, "is_ai_playground": false,
			"use_deep_think": false, "use_auto_cot": false, "resend_for_regen": false, "enable_commerce_credit": false,
		},
		"evaluate_option":       map[string]string{"web_ab_params": ""},
		"local_conversation_id": randomUUID(), "local_message_id": randomUUID(),
	}
}

func (c *Client) mediaCall(ctx context.Context, jar cookieJar, payload map[string]any, video bool) ([]generatedMedia, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	target := c.baseURL() + "/samantha/chat/completion?" + c.commonQuery(c.acc, fingerprintQuery(jar))
	upstream.NoteUpstreamCall(ctx, target, "")
	response, err := c.doJSON(ctx, http.MethodPost, target, body, jar, false, "application/json", "text/event-stream")
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		err = mediaError(response.StatusCode, raw, video)
		c.noteVerification(ctx, err)
		return nil, "", err
	}
	media, task, err := consumeMediaStream(response.Body, video)
	if err != nil {
		c.noteVerification(ctx, err)
	}
	return media, task, err
}

func mediaError(status int, data []byte, video bool) error {
	if video && videoQuotaMessage(string(data)) {
		return errVideoQuota
	}
	return statusError(status, data)
}

func videoQuotaMessage(s string) bool {
	for _, substring := range []string{"今日生成次数已达上限", "次数已达上限", "额度已用尽", "达到今日上限", "次数超限"} {
		if strings.Contains(s, substring) {
			return true
		}
	}
	return false
}

func consumeMediaStream(r io.Reader, video bool) ([]generatedMedia, string, error) {
	var media []generatedMedia
	seen := map[string]bool{}
	var task string
	var quota bool
	err := readDoubaoEvents(r, true, func(_ string, data []byte) error {
		if len(data) == 0 {
			return nil
		}
		var event map[string]any
		if err := json.Unmarshal(data, &event); err != nil {
			return errNoMedia
		}
		if event["code"] != nil && event["event_type"] == nil {
			raw, _ := json.Marshal(event)
			return mediaError(502, raw, video)
		}
		detail := mediaObject(event["event_data"])
		switch mediaNumber(event["event_type"]) {
		case 2005:
			raw, _ := json.Marshal(event)
			if task != "" {
				return mediaError(502, raw, false)
			}
			return mediaError(502, raw, video)
		case 2001:
			if fin := mediaObject(detail["fin_reason"]); mediaNumber(fin["reason"]) == 1 {
				if id, ok := mediaObject(fin["async_task"])["id"].(string); ok && id != "" && len(id) <= 256 {
					task = id
				}
			}
			msg := mediaObject(detail["message"])
			content := mediaObject(msg["content"])
			kind := mediaNumber(msg["content_type"])
			if video && kind == 2001 {
				if value, _ := content["text"].(string); videoQuotaMessage(value) {
					quota = true
				}
			}
			if (!video && kind != 2010) || (video && kind != 2021) {
				return nil
			}
			items, ok := content["data"].([]any)
			if !ok {
				items = []any{content}
			}
			for _, item := range items {
				entry := parseGeneratedMedia(mediaObject(item), video)
				if entry.URL != "" && !seen[entry.URL] && len(media) < maxAttachments {
					media = append(media, entry)
					seen[entry.URL] = true
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if video && quota && len(media) == 0 && task == "" {
		return nil, "", errVideoQuota
	}
	return media, task, nil
}

func mediaObject(v any) map[string]any {
	switch value := v.(type) {
	case map[string]any:
		return value
	case string:
		var obj map[string]any
		_ = json.Unmarshal([]byte(value), &obj)
		return obj
	}
	return nil
}

func mediaNumber(v any) float64 {
	number, _ := v.(float64)
	return number
}

func safeMediaURL(raw any) string {
	s, _ := raw.(string)
	u, err := httpclient.ValidateRemoteFetchURL(s)
	if err != nil || u.Scheme != "https" {
		return ""
	}
	return s
}

func parseGeneratedMedia(item map[string]any, video bool) generatedMedia {
	m := generatedMedia{Width: int(mediaNumber(item["width"])), Height: int(mediaNumber(item["height"]))}
	if video {
		m.URL = first(safeMediaURL(item["video_url"]), safeMediaURL(item["url"]))
		m.CoverURL = first(safeMediaURL(item["cover_url"]), safeMediaURL(mediaObject(item["cover"])["url"]))
		m.Duration = mediaNumber(item["duration"])
		if m.URL == "" {
			variants := mediaObject(mediaObject(item["video_model"])["video_list"])
			keys := make([]string, 0, len(variants))
			for key := range variants {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			for _, key := range keys {
				main, _ := mediaObject(variants[key])["main_url"].(string)
				decoded, err := base64.StdEncoding.DecodeString(main)
				if err == nil {
					m.URL = safeMediaURL(string(decoded))
				}
				if m.URL != "" {
					break
				}
			}
		}
		return m
	}
	for _, key := range []string{"image_ori", "image_raw", "image_thumb"} {
		variant := mediaObject(item[key])
		if m.URL = safeMediaURL(variant["url"]); m.URL != "" {
			m.Width, m.Height = int(mediaNumber(variant["width"])), int(mediaNumber(variant["height"]))
			break
		}
	}
	return m
}
