package web

import (
	"net/http"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/util"
)

func (h *Handler) HandleAdminImagineConfig(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodGet) {
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

	util.WriteJSON(w, map[string]any{
		"final_min_bytes":  finalMinBytes,
		"medium_min_bytes": mediumMinBytes,
		"nsfw":             nsfw,
	})
}
