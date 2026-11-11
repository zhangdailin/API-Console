package main

import (
	"net/http"
	"strings"

	"orchids-api/internal/grok"
	"orchids-api/internal/grok/web"
	"orchids-api/internal/handler"
	"orchids-api/internal/middleware"
)

// adminToolsOwner scopes the stateful resources (video jobs, media inputs,
// stored responses) the admin tools page creates, so they are not pooled with
// keyless callers and stay stable across the operator's browsers.
const adminToolsOwner = "admin-tools"

// webVideoResource serves GET /videos/{id} and GET /videos/{id}/content.
func webVideoResource(h *web.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/content") {
			h.HandleVideosContent(w, r)
			return
		}
		h.HandleVideosRetrieve(w, r)
	}
}

// registerGrokWebAdminRoutes mounts the Grok Web management surface: SSO
// account import and maintenance, and the Grok tools page (Imagine, video,
// voice, media cache). Everything here sits behind the admin session.
func registerGrokWebAdminRoutes(mux *http.ServeMux, sessionAuth func(http.HandlerFunc) http.HandlerFunc, h *handler.Handler, grokHandler *grok.Handler, flowTracker *middleware.FlowTracker) {
	webHandler := grokHandler.Web()

	// SSO accounts. Cookies are write-only; responses name accounts by ID.
	mux.HandleFunc("/api/grok/sso/import", sessionAuth(webHandler.HandleSSOImport))
	mux.HandleFunc("/api/grok/sso/refresh", sessionAuth(webHandler.HandleSSORefresh))
	mux.HandleFunc("/api/grok/sso/nsfw", sessionAuth(webHandler.HandleSSONSFW))
	mux.HandleFunc("/api/grok/availability", sessionAuth(webHandler.HandleAvailability))

	// The tools page is an authenticated management-plane projection of the
	// grok channel's inference handlers: the operator's session is the
	// authorization boundary, and its resources carry a dedicated owner scope.
	withAdminToolsOwner := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			next(w, r.WithContext(middleware.WithAPIKeyFingerprint(r.Context(), adminToolsOwner)))
		}
	}
	// The handlers resolve their channel from the path, so a tools request is
	// presented to them under the grok channel prefix it projects.
	const toolsPrefix = "/api/grok/tools/v1"
	asGrokChannel := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			rewritten := r.Clone(r.Context())
			rewritten.URL.Path = "/grok/v1" + strings.TrimPrefix(r.URL.Path, toolsPrefix)
			rewritten.URL.RawPath = ""
			next(w, rewritten)
		}
	}
	// Tools traffic reaches the same upstream accounts as API-key traffic, so
	// the live flow screen counts it too. The tracker runs after the rewrite:
	// it reads the provider and protocol from the /grok/v1 path.
	toolRoute := func(path string, next http.HandlerFunc) {
		mux.HandleFunc(toolsPrefix+path, sessionAuth(withAdminToolsOwner(asGrokChannel(flowTracker.Track(withToolsLinks(next))))))
	}
	// File-upload routes accept multipart/form-data from the admin session.
	uploadToolRoute := func(path string, next http.HandlerFunc) {
		mux.HandleFunc(toolsPrefix+path, middleware.AllowMultipartAdminWrite(sessionAuth(withAdminToolsOwner(asGrokChannel(flowTracker.Track(withToolsLinks(next)))))))
	}
	toolRoute("/models", h.HandleModels)
	toolRoute("/chat/completions", grokHandler.HandleChatCompletions)
	toolRoute("/responses", grokHandler.HandleResponses)
	toolRoute("/responses/", grokHandler.HandleResponseResource)
	toolRoute("/images/generations", webHandler.HandleImagesGenerations)
	uploadToolRoute("/images/edits", webHandler.HandleImagesEdits)
	toolRoute("/videos", webHandler.HandleVideosCreate)
	toolRoute("/videos/", webVideoResource(webHandler))
	toolRoute("/files/", webHandler.HandleFiles)
	uploadToolRoute("/media/inputs", webHandler.HandleMediaInputs)
	toolRoute("/media/inputs/import", webHandler.HandleMediaInputImport)
	toolRoute("/media/inputs/", webHandler.HandleMediaInputResource)

	adminRoutes := []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/tokens/nsfw/enable", webHandler.HandleAdminNSFWEnable},
		{"/tokens/nsfw/enable/async", webHandler.HandleAdminNSFWEnableAsync},
		{"/batch/", webHandler.HandleAdminBatchTask},
		{"/media/images", webHandler.HandleAdminMediaImages},
		{"/media/images/stats", webHandler.HandleAdminMediaImageStats},
		{"/media/images/delete", webHandler.HandleAdminMediaImagesDelete},
		{"/media/images/content/", webHandler.HandleAdminMediaImageContent},
		{"/media/videos", webHandler.HandleAdminMediaVideos},
		{"/media/videos/stats", webHandler.HandleAdminMediaVideoStats},
		{"/media/videos/delete", webHandler.HandleAdminMediaVideoDelete},
		{"/media/videos/content/", webHandler.HandleAdminMediaVideoContent},
		{"/media/inputs/upload", webHandler.HandleAdminMediaInputs},
		{"/media/inputs/import", webHandler.HandleAdminMediaInputImport},
		{"/media/inputs/", webHandler.HandleAdminMediaInputResource},
		{"/voice/token", webHandler.HandleAdminVoiceToken},
		// LiveKit signal relay for browsers that cannot reach livekit.grok.com.
		{"/voice/livekit/", webHandler.HandleAdminVoiceRelay},
		{"/imagine/config", webHandler.HandleAdminImagineConfig},
	}
	for _, route := range adminRoutes {
		handler := sessionAuth(withAdminToolsOwner(withToolsLinks(route.handler)))
		if route.path == "/media/inputs/upload" {
			handler = middleware.AllowMultipartAdminWrite(handler)
		}
		mux.HandleFunc("/api/grok/admin"+route.path, handler)
	}
}
