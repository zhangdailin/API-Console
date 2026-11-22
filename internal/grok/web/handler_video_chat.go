package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/debug"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/util"
)

type videoGenerationResult struct {
	URL         string
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

func (h *Handler) runVideoGeneration(
	ctx context.Context,
	sess *chatAccountSession,
	spec ModelSpec,
	prompt string,
	attachments []AttachmentInput,
	cfg *VideoConfig,
	logger *debug.Logger,
	onProgress func(int),
) (videoGenerationResult, error) {
	if cfg.VideoLength < 1 || cfg.VideoLength > 15 {
		return videoGenerationResult{}, fmt.Errorf("video_length must be between 1 and 15 seconds")
	}
	payload, err := buildVideoCreatePayload(spec, prompt, attachments, *cfg)
	if err != nil {
		return videoGenerationResult{}, err
	}
	if h == nil || h.webClient() == nil {
		return videoGenerationResult{}, fmt.Errorf("grok client not configured")
	}
	if logger != nil && !logger.Capturing() {
		logger.LogUpstreamRequest(h.webClient().baseURL()+defaultChatPath, debugHeaderMap(h.webClient().headers(sess.token)), payload)
	}
	// Video creation has no account-specific inputs; a switch reuses the payload.
	resp, err := h.doChatWithAutoSwitchRebuild(ctx, sess, &payload, nil)
	if err != nil {
		return videoGenerationResult{}, err
	}
	defer resp.Body.Close()
	h.syncGrokQuota(sess.acc, resp.Header)
	return collectVideoResult(resp.Body, logger, onProgress)
}

func collectVideoResult(body io.Reader, logger *debug.Logger, onProgress func(int)) (videoGenerationResult, error) {
	var result videoGenerationResult
	var assetID string
	err := parseUpstreamLines(body, func(resp map[string]any) error {
		logger.LogOutputSSEJSON("response", resp)
		if postID := extractVideoPostID(resp); postID != "" {
			result.VideoPostID = postID
		}
		if assetID == "" {
			assetID = firstNonEmpty(extractAssetIDs(resp)...)
		}
		progress, videoURL, ok := extractVideoProgress(resp)
		if !ok {
			return nil
		}
		if onProgress != nil {
			onProgress(clampProgress(progress))
		}
		if progress >= 100 && strings.TrimSpace(videoURL) != "" {
			result.URL = strings.TrimSpace(videoURL)
		}
		return nil
	})
	if err != nil {
		return videoGenerationResult{}, err
	}
	if result.URL == "" {
		result.URL = assetURLFromAssetID(assetID)
	}
	if strings.TrimSpace(result.VideoPostID) == "" {
		result.VideoPostID = firstNonEmpty(assetID, result.URL)
	}
	if strings.TrimSpace(result.URL) == "" {
		return videoGenerationResult{}, fmt.Errorf("video generation returned no final video URL")
	}
	return result, nil
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
	flusher := httpserver.StreamResponseHeaders(w)
	id := "chatcmpl_" + randomHex(8)
	chunkScratch := make([]byte, 0, 256)
	emit := func(content string, finish string, hasFinish bool, usage map[string]any) {
		raw := appendChatCompletionChunkWithUsage(chunkScratch[:0], id, time.Now().Unix(), req.Model, "", "", content, finish, hasFinish, usage)
		chunkScratch = raw[:0]
		writeSSELog(w, flusher, logger, raw)
	}
	role := appendChatCompletionChunkWithUsage(chunkScratch[:0], id, time.Now().Unix(), req.Model, "", "assistant", "", "", false, nil)
	chunkScratch = role[:0]
	httpserver.WriteSSEBytes(w, "", role)
	lastProgress := -1
	artifact, err := h.runVideoGeneration(ctx, sess, spec, prompt, attachments, req.VideoConfig, logger, func(progress int) {
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
	if ctx.Err() != nil {
		return
	}
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
	artifact, err := h.runVideoGeneration(ctx, sess, spec, prompt, attachments, req.VideoConfig, logger, func(progress int) {
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
	if ctx.Err() != nil {
		return
	}
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
	util.WriteJSON(w, resp)
}
