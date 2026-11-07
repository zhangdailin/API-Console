package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/debug"
)

type videoSegmentResult struct {
	URL         string
	AssetID     string
	VideoPostID string
}

func (h *Handler) serveVideoChatCompletion(
	ctx context.Context,
	w http.ResponseWriter,
	req *ChatCompletionsRequest,
	spec ModelSpec,
	prompt string,
	attachments []AttachmentInput,
	publicBase string,
	logger *debug.Logger,
) {
	sess, err := h.openChatAccountSessionForModel(ctx, spec)
	if err != nil {
		writeGrokNoAccountError(w, err)
		return
	}
	defer sess.Close()

	if req != nil && req.Stream {
		h.streamVideoChatCompletion(ctx, w, req, spec, prompt, attachments, publicBase, sess, logger)
		return
	}
	h.collectVideoChatCompletion(ctx, w, req, spec, prompt, attachments, publicBase, sess, logger)
}

func (h *Handler) runVideoSegment(
	ctx context.Context,
	sess *chatAccountSession,
	payload map[string]any,
	rebuild func(token string) (map[string]any, error),
	logger *debug.Logger,
	onProgress func(int),
) (videoSegmentResult, error) {
	if h == nil || h.webClient() == nil {
		return videoSegmentResult{}, fmt.Errorf("grok client not configured")
	}
	if logger != nil {
		if !logger.Capturing() {
			logger.LogUpstreamRequest(h.webClient().baseURL()+defaultChatPath, debugHeaderMap(h.webClient().headers(sess.token)), payload)
		}
	}
	resp, err := h.doChatWithAutoSwitchRebuild(ctx, sess, &payload, rebuild)
	if err != nil {
		return videoSegmentResult{}, err
	}
	defer resp.Body.Close()
	h.syncGrokQuota(sess.acc, resp.Header)
	return h.collectVideoSegmentFromBody(resp.Body, logger, onProgress)
}

func (h *Handler) collectVideoSegmentFromBody(body io.Reader, logger *debug.Logger, onProgress func(int)) (videoSegmentResult, error) {
	var result videoSegmentResult
	err := parseUpstreamLines(body, func(resp map[string]any) error {
		if logger != nil {
			if raw, err := json.Marshal(resp); err == nil {
				logger.LogOutputSSE("response", string(raw))
			}
		}
		if postID := extractVideoPostID(resp); postID != "" {
			result.VideoPostID = postID
		}
		if result.AssetID == "" {
			result.AssetID = firstNonEmpty(extractAssetIDs(resp)...)
		}
		progress, videoURL, ok := extractVideoProgress(resp)
		if !ok {
			return nil
		}
		if onProgress != nil {
			onProgress(progress)
		}
		if progress >= 100 && strings.TrimSpace(videoURL) != "" {
			result.URL = strings.TrimSpace(videoURL)
		}
		return nil
	})
	if err != nil {
		return videoSegmentResult{}, err
	}
	if strings.TrimSpace(result.URL) == "" && strings.TrimSpace(result.AssetID) != "" {
		result.URL = assetURLFromAssetID(result.AssetID)
	}
	if strings.TrimSpace(result.VideoPostID) == "" {
		result.VideoPostID = firstNonEmpty(result.AssetID, result.URL)
	}
	if strings.TrimSpace(result.URL) == "" {
		return videoSegmentResult{}, fmt.Errorf("video generation returned no final video URL")
	}
	return result, nil
}

func videoParentPostIDFromPayload(payload map[string]any) string {
	respMeta, _ := payload["responseMetadata"].(map[string]any)
	modelCfg, _ := respMeta["modelConfigOverride"].(map[string]any)
	modelMap, _ := modelCfg["modelMap"].(map[string]any)
	videoCfg, _ := modelMap["videoGenModelConfig"].(map[string]any)
	return strings.TrimSpace(fmt.Sprint(videoCfg["parentPostId"]))
}

func (h *Handler) runVideoSegments(
	ctx context.Context,
	sess *chatAccountSession,
	spec ModelSpec,
	prompt string,
	attachments []AttachmentInput,
	cfg *VideoConfig,
	logger *debug.Logger,
	onProgress func(int),
) (videoSegmentResult, error) {
	segments, err := videoSegmentLengths(cfg.VideoLength)
	if err != nil {
		return videoSegmentResult{}, err
	}
	total := len(segments)
	parentPostID := ""
	extendPostID := ""
	elapsed := 0
	var artifact videoSegmentResult

	for idx, length := range segments {
		segmentCfg := *cfg
		segmentCfg.VideoLength = length
		var payload map[string]any
		var rebuild func(string) (map[string]any, error)
		if idx == 0 {
			payload, err = h.buildVideoCreatePayload(ctx, sess.token, spec, prompt, attachments, &segmentCfg)
			if err != nil {
				return videoSegmentResult{}, err
			}
			rebuild = func(token string) (map[string]any, error) {
				return h.buildVideoCreatePayload(ctx, token, spec, prompt, attachments, &segmentCfg)
			}
		} else {
			payload = h.buildVideoExtendPayload(spec, prompt, parentPostID, extendPostID, &segmentCfg, length, elapsed)
			rebuild = func(string) (map[string]any, error) {
				return h.buildVideoExtendPayload(spec, prompt, parentPostID, extendPostID, &segmentCfg, length, elapsed), nil
			}
		}

		segmentIndex := idx
		artifact, err = h.runVideoSegment(ctx, sess, payload, rebuild, logger, func(progress int) {
			if onProgress == nil {
				return
			}
			scaled := int(((float64(segmentIndex) + float64(clampProgress(progress))/100.0) / float64(total)) * 100)
			onProgress(scaled)
		})
		if err != nil {
			return videoSegmentResult{}, err
		}
		if idx == 0 {
			parentPostID = videoParentPostIDFromPayload(payload)
			if total > 1 {
				artifact.VideoPostID = firstNonEmpty(artifact.VideoPostID, parentPostID)
			}
		}
		extendPostID = firstNonEmpty(artifact.VideoPostID, artifact.AssetID, parentPostID)
		elapsed += length
	}
	return artifact, nil
}

func clampProgress(progress int) int {
	return min(max(progress, 0), 100)
}

func (h *Handler) videoOutputURL(ctx context.Context, token, rawURL, publicBase string) string {
	out := strings.TrimSpace(rawURL)
	if out == "" {
		return ""
	}
	if name, err := h.cacheMediaURL(ctx, token, out, "video"); err == nil && name != "" {
		out = "/grok/v1/files/video/" + name
	} else if err != nil {
		slog.Warn("grok video convert failed", "url", rawURL, "error", err)
	}
	if publicBase != "" && strings.HasPrefix(out, "/") {
		out = publicBase + out
	}
	return out
}

func (h *Handler) streamVideoChatCompletion(
	ctx context.Context,
	w http.ResponseWriter,
	req *ChatCompletionsRequest,
	spec ModelSpec,
	prompt string,
	attachments []AttachmentInput,
	publicBase string,
	sess *chatAccountSession,
	logger *debug.Logger,
) {
	flusher := streamResponseHeaders(w)
	id := "chatcmpl_" + randomHex(8)
	chunkScratch := make([]byte, 0, 256)
	emit := func(content string, finish string, hasFinish bool, usage map[string]any) {
		raw := appendChatCompletionChunkWithUsage(chunkScratch[:0], id, time.Now().Unix(), req.Model, "", "", content, finish, hasFinish, usage)
		chunkScratch = raw[:0]
		writeSSELog(w, flusher, logger, raw)
	}
	role := appendChatCompletionChunkWithUsage(chunkScratch[:0], id, time.Now().Unix(), req.Model, "", "assistant", "", "", false, nil)
	chunkScratch = role[:0]
	writeSSEBytes(w, "", role)
	lastProgress := -1
	artifact, err := h.runVideoSegments(ctx, sess, spec, prompt, attachments, req.VideoConfig, logger, func(progress int) {
		progress = clampProgress(progress)
		if progress <= lastProgress {
			return
		}
		lastProgress = progress
		// Progress is not the answer: it goes to reasoning_content, as the
		// non-streaming response reports it, and the content is the video URL.
		raw := appendChatCompletionReasoningChunk(chunkScratch[:0], id, time.Now().Unix(), req.Model, "", fmt.Sprintf("视频正在生成 %d%%\n", progress))
		chunkScratch = raw[:0]
		writeSSELog(w, flusher, logger, raw)
	})
	if err != nil {
		writeSSECodedError(w, flusher, err.Error(), "video_generation_failed")
		return
	}
	content := h.videoOutputURL(ctx, sess.token, artifact.URL, publicBase)
	emit(content, "", false, nil)
	emit("", "stop", true, buildChatUsagePayload(req, content, nil))
	writeSSE(w, flusher, "", []byte("[DONE]"))
}

func (h *Handler) collectVideoChatCompletion(
	ctx context.Context,
	w http.ResponseWriter,
	req *ChatCompletionsRequest,
	spec ModelSpec,
	prompt string,
	attachments []AttachmentInput,
	publicBase string,
	sess *chatAccountSession,
	logger *debug.Logger,
) {
	reasoning := make([]string, 0, 8)
	lastProgress := -1
	artifact, err := h.runVideoSegments(ctx, sess, spec, prompt, attachments, req.VideoConfig, logger, func(progress int) {
		progress = clampProgress(progress)
		if progress <= lastProgress {
			return
		}
		lastProgress = progress
		reasoning = append(reasoning, fmt.Sprintf("视频正在生成 %d%%", progress))
	})
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	content := h.videoOutputURL(ctx, sess.token, artifact.URL, publicBase)
	resp := map[string]any{
		"id":                 "chatcmpl_" + randomHex(8),
		"object":             "chat.completion",
		"created":            time.Now().Unix(),
		"model":              req.Model,
		"service_tier":       nil,
		"system_fingerprint": "",
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":        "assistant",
					"content":     content,
					"refusal":     nil,
					"annotations": []any{},
				},
				"finish_reason": "stop",
			},
		},
		"usage": buildChatUsagePayload(req, content, nil),
	}
	if len(reasoning) > 0 {
		choice := resp["choices"].([]map[string]any)[0]
		message := choice["message"].(map[string]any)
		message["reasoning_content"] = strings.Join(reasoning, "\n")
	}
	writeJSON(w, resp)
}
