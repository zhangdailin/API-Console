package web

import "net/http"

func ensureImageNSFW(payload map[string]interface{}) {
	if payload == nil {
		return
	}
	modelConfigOverride, _ := payload["modelConfigOverride"].(map[string]interface{})
	if modelConfigOverride == nil {
		return
	}
	modelMap, _ := modelConfigOverride["modelMap"].(map[string]interface{})
	if modelMap == nil {
		return
	}
	imageGenCfg, _ := modelMap["imageGenModelConfig"].(map[string]interface{})
	if imageGenCfg == nil {
		return
	}
	delete(imageGenCfg, "enableNsfw")
	delete(imageGenCfg, "enable_nsfw")
}

func (h *Handler) HandleAdminImagineConfig(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	finalMinBytes := 100000
	mediumMinBytes := 30000
	nsfw := true
	if h != nil && h.configSnapshot() != nil {
		finalMinBytes = h.configSnapshot().PublicImagineFinalMinBytes()
		mediumMinBytes = h.configSnapshot().PublicImagineMediumMinBytes()
		nsfw = h.configSnapshot().PublicImagineNSFW()
	}

	writeJSON(w, map[string]interface{}{
		"final_min_bytes":  finalMinBytes,
		"medium_min_bytes": mediumMinBytes,
		"nsfw":             nsfw,
	})
}
