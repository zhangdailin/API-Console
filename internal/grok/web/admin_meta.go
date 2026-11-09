package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/util"
	"strings"
)

func (h *Handler) HandleAdminVoiceToken(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}

	var body struct {
		Voice       string  `json:"voice"`
		Personality string  `json:"personality"`
		Speed       float64 `json:"speed"`
		Instruction string  `json:"instruction"`
	}
	if r.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				httpserver.WriteError(w, http.StatusBadRequest, "invalid voice token request")
				return
			}
		}
	}

	voice := strings.TrimSpace(body.Voice)
	if voice == "" {
		voice = "ara"
	}
	personality := strings.TrimSpace(body.Personality)
	if personality == "" {
		personality = "assistant"
	}
	speed := body.Speed
	if speed <= 0 {
		speed = 1.0
	}
	instruction := strings.TrimSpace(body.Instruction)

	acc, token, err := h.selectAccount(r.Context())
	if err != nil {
		writeGrokNoAccountError(w, err)
		return
	}

	client := h.currentClient()
	if client == nil {
		httpserver.WriteError(w, http.StatusServiceUnavailable, "grok client not configured")
		return
	}
	data, err := client.getVoiceToken(r.Context(), token, voice, personality, speed, instruction)
	if err != nil {
		h.markAccountStatus(r.Context(), acc, err)
		writeGrokUpstreamError(w, err)
		return
	}
	respToken, _ := data["token"].(string)
	respToken = strings.TrimSpace(respToken)
	if respToken == "" {
		httpserver.WriteError(w, http.StatusBadGateway, "upstream returned no voice token")
		return
	}

	out := map[string]any{
		"token":            respToken,
		"url":              firstVoiceString(data, "livekitUrl", "url"),
		"participant_name": firstVoiceString(data, "participantName", "participant_name", "identity"),
		"room_name":        firstVoiceString(data, "roomName", "room_name", "room"),
	}
	if strings.TrimSpace(out["url"].(string)) == "" {
		out["url"] = "wss://livekit.grok.com"
	}
	util.WriteJSON(w, out)
}

func firstVoiceString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(strings.Trim(strings.TrimSpace(fmt.Sprint(data[key])), `"`)); value != "" && value != "<nil>" {
			return value
		}
	}
	return ""
}
