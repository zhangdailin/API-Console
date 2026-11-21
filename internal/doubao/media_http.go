package doubao

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/config"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

type MediaHandler struct {
	store   *store.Store
	config  func() *config.Config
	acquire func(context.Context, string, []int64) (*store.Account, func(), error)
}

func NewMediaHandler(s *store.Store, cfg func() *config.Config, acquire func(context.Context, string, []int64) (*store.Account, func(), error)) *MediaHandler {
	return &MediaHandler{store: s, config: cfg, acquire: acquire}
}

func (h *MediaHandler) Images(w http.ResponseWriter, r *http.Request) { h.generate(w, r, ImageModel) }
func (h *MediaHandler) ImageEdits(w http.ResponseWriter, r *http.Request) {
	h.generate(w, r, ImageEditModel)
}
func (h *MediaHandler) Videos(w http.ResponseWriter, r *http.Request) { h.generate(w, r, VideoModel) }

const mediaRequestLimit = 24 << 20

func (h *MediaHandler) generate(w http.ResponseWriter, r *http.Request, model string) {
	if !requirePost(w, r) {
		return
	}
	req, inputs, err := parseMediaRequest(w, r, model)
	if err != nil {
		writeInputError(w, err, "Invalid Doubao media request")
		return
	}
	if !middleware.APIKeyAllowsModel(r.Context(), req.Model) {
		writeDoubaoError(w, http.StatusForbidden, "permission_error", "API key is not allowed to use this Doubao model")
		return
	}
	if middleware.APIKeyHasBillingLimit(r.Context()) {
		// Doubao does not publish a tariff or billable usage for these
		// operations. A capped key must not get an unmetered media route.
		writeDoubaoError(w, http.StatusPaymentRequired, "media_pricing_unavailable", "Doubao media pricing is unavailable for billing-limited API keys")
		return
	}
	cfg := h.config()
	ctx, cancel := context.WithTimeout(middleware.WithRequestModel(r.Context(), req.Model), 6*time.Minute)
	defer cancel()
	budget := store.DoubaoFileLimit
	for _, input := range inputs {
		ref, err := input.resolve(ctx, h.store, cfg, budget)
		if err != nil {
			writeInputError(w, err, "Invalid or unavailable Doubao reference image")
			return
		}
		req.References = append(req.References, ref)
		budget -= len(ref.data)
	}
	// A definite pre-acceptance video quota rejection permits one alternate
	// account. Transport failures and accepted tasks are never replayed.
	maxAttempts := 1
	if model == VideoModel {
		maxAttempts = 2
	}
	var excluded []int64
	for attempt := 0; attempt < maxAttempts; attempt++ {
		account, release, err := h.acquire(ctx, req.Model, excluded)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				writeDoubaoError(w, http.StatusGatewayTimeout, "upstream_timeout", "Doubao media request timed out")
			} else if len(excluded) > 0 || errors.Is(err, store.ErrDoubaoVideoCooldown) {
				writeDoubaoError(w, http.StatusTooManyRequests, "rate_limit_error", "No Doubao account has available video quota")
			} else {
				writeDoubaoError(w, http.StatusServiceUnavailable, "service_unavailable", "No Doubao account is available for this request")
			}
			return
		}
		client := newClientWithStore(account, cfg, h.store)
		media, err := client.generateMedia(ctx, req)
		var challenge *verificationRequired
		if errors.As(err, &challenge) {
			at := time.Now()
			if saveErr := h.store.SetDoubaoVerificationPark(ctx, client.acc, challenge.Error(), at, at.Add(accountpolicy.CooldownVerification)); saveErr != nil {
				slog.Warn("Doubao media verification observation was not saved", "account_id", account.ID)
			}
		} else if errors.Is(err, errSessionRejected) && h.store != nil {
			if saveErr := h.store.UpdateDoubaoSession(ctx, account.ID, store.DoubaoSessionPatch{
				Expected: client.sessionVersion, Rejected: true, ObservedAt: time.Now(),
			}); saveErr != nil {
				slog.Warn("Doubao media session rejection was not saved", "account_id", account.ID)
			}
		}
		if errors.Is(err, errVideoQuota) {
			// Persist while still holding the slot so the next account
			// selection sees the same quota verdict.
			if saveErr := h.store.SetDoubaoVideoCooldown(ctx, client.acc, VideoModel, nextVideoQuotaDay(time.Now())); saveErr != nil {
				release()
				writeDoubaoError(w, http.StatusServiceUnavailable, "service_unavailable", "Could not record Doubao video quota")
				return
			}
		}
		release()
		switch {
		case errors.Is(err, errVideoQuota):
			// Record a feature-specific cooldown; general chat and images still
			// use the account. Shanghai midnight is the local daily boundary.
			excluded = append(excluded, account.ID)
			continue
		case err != nil:
			writeMediaError(w, err)
			return
		}
		if req.Format == "b64_json" {
			budget := store.DoubaoFileLimit
			for i := range media {
				data, _, err := fetchAttachment(ctx, cfg, media[i].URL, budget)
				_, valid := imageNameFromBytes(data)
				if err != nil || !valid {
					writeDoubaoError(w, http.StatusBadGateway, "upstream_error", "Could not read the generated Doubao image")
					return
				}
				budget -= len(data)
				media[i].Base64 = base64.StdEncoding.EncodeToString(data)
				media[i].URL = ""
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		if model == VideoModel {
			data := make([]map[string]any, 0, len(media))
			for _, m := range media {
				entry := map[string]any{"video_url": m.URL}
				if m.CoverURL != "" {
					entry["cover_url"] = m.CoverURL
				}
				if m.Width > 0 {
					entry["width"] = m.Width
				}
				if m.Height > 0 {
					entry["height"] = m.Height
				}
				if m.Duration > 0 {
					entry["duration"] = m.Duration
				}
				data = append(data, entry)
			}
			util.WriteJSONStatus(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
			return
		}
		util.WriteJSONStatus(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": media})
		return
	}
	writeDoubaoError(w, http.StatusTooManyRequests, "rate_limit_error", "Doubao daily video quota exhausted")
}

func nextVideoQuotaDay(now time.Time) time.Time {
	local := now.In(time.FixedZone("Asia/Shanghai", 8*60*60))
	return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, local.Location())
}

func writeMediaError(w http.ResponseWriter, err error) {
	var challenge *verificationRequired
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		writeDoubaoError(w, http.StatusGatewayTimeout, "upstream_timeout", "Doubao media request timed out")
	case errors.Is(err, errNoMedia):
		writeDoubaoError(w, http.StatusBadGateway, "upstream_no_result", "Doubao did not return a completed media result")
	case errors.As(err, &challenge):
		writeDoubaoError(w, http.StatusServiceUnavailable, "verification_required", "The Doubao account requires human verification")
	case strings.HasPrefix(err.Error(), "status=429"):
		writeDoubaoError(w, http.StatusTooManyRequests, "rate_limit_error", "Doubao limited the media request")
	default:
		writeDoubaoError(w, http.StatusBadGateway, "upstream_error", "Doubao media generation failed")
	}
}

func requirePost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodPost {
		return true
	}
	w.Header().Set("Allow", "POST")
	writeDoubaoError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
	return false
}

func writeDoubaoError(w http.ResponseWriter, status int, code, message string) {
	apperrors.New(code, message, status).WriteResponse(w)
}

func writeInputError(w http.ResponseWriter, err error, message string) {
	var size *http.MaxBytesError
	if errors.As(err, &size) {
		writeDoubaoError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "Doubao upload is too large")
		return
	}
	writeDoubaoError(w, http.StatusBadRequest, "invalid_request_error", message)
}

type imageInput struct {
	raw, fileID string
	data        []byte
}

func (i imageInput) resolve(ctx context.Context, s *store.Store, cfg *config.Config, budget int) (promptBlock, error) {
	var err error
	data := i.data
	switch {
	case i.fileID != "":
		file, loadErr := loadFile(ctx, s, i.fileID)
		if loadErr != nil || !file.Image {
			return promptBlock{}, errInvalidAttachment
		}
		data = file.Data
	case isRemoteAttachment(i.raw):
		data, _, err = fetchAttachment(ctx, cfg, i.raw, budget)
	case i.raw != "":
		data, err = decodeBase64File(i.raw)
	}
	name, ok := imageNameFromBytes(data)
	if err != nil {
		return promptBlock{}, err
	}
	if len(data) > budget {
		return promptBlock{}, &http.MaxBytesError{Limit: store.DoubaoFileLimit}
	}
	if !ok {
		return promptBlock{}, errInvalidAttachment
	}
	return promptBlock{data: data, name: name, image: true}, nil
}

func parseMediaRequest(w http.ResponseWriter, r *http.Request, model string) (mediaRequest, []imageInput, error) {
	r.Body = http.MaxBytesReader(w, r.Body, mediaRequestLimit)
	req := mediaRequest{Model: model, Count: 1, Format: "url"}
	var images []imageInput
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "multipart/form-data" {
		reader, err := r.MultipartReader()
		if err != nil {
			return req, nil, err
		}
		var total int
		for count := 0; ; count++ {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return req, nil, err
			}
			if count == 16 {
				return req, nil, errInvalidAttachment
			}
			name := part.FormName()
			if name == "image" || name == "image[]" || name == "reference_image" {
				if len(images) >= maxAttachments {
					return req, nil, errInvalidAttachment
				}
				if part.FileName() == "" {
					raw, err := io.ReadAll(io.LimitReader(part, 4097))
					if err != nil || len(raw) > 4096 {
						return req, nil, errInvalidAttachment
					}
					images = append(images, namedImageInput(strings.TrimSpace(string(raw))))
				} else {
					data, err := readAttachment(part, store.DoubaoFileLimit-total)
					if err != nil {
						return req, nil, err
					}
					images = append(images, imageInput{data: data})
					total += len(data)
				}
			} else {
				data, err := io.ReadAll(io.LimitReader(part, 16385))
				if err != nil || len(data) > 16384 {
					return req, nil, errInvalidAttachment
				}
				value := strings.TrimSpace(string(data))
				switch name {
				case "prompt":
					req.Prompt = value
				case "model":
					if value != "" {
						req.Model = value
					}
				case "size", "ratio", "aspect_ratio":
					req.Ratio = value
				case "response_format":
					req.Format = value
				case "n":
					req.Count, _ = strconv.Atoi(value)
				default:
					return req, nil, errInvalidAttachment
				}
			}
			part.Close()
		}
	} else {
		var fields struct {
			Model           string          `json:"model"`
			Prompt          string          `json:"prompt"`
			Ratio           string          `json:"ratio"`
			AspectRatio     string          `json:"aspect_ratio"`
			Size            string          `json:"size"`
			ResponseFormat  string          `json:"response_format"`
			Count           *int            `json:"n"`
			Image           json.RawMessage `json:"image"`
			Images          json.RawMessage `json:"images"`
			ReferenceImages json.RawMessage `json:"reference_images"`
		}
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&fields); err != nil {
			return req, nil, err
		}
		if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
			if err == nil {
				err = errInvalidAttachment
			}
			return req, nil, err
		}
		req.Model = first(fields.Model, model)
		req.Prompt, req.Ratio, req.Format = fields.Prompt, first(fields.Ratio, fields.AspectRatio, fields.Size), first(fields.ResponseFormat, "url")
		if fields.Count != nil {
			req.Count = *fields.Count
		}
		for _, raw := range []json.RawMessage{fields.Image, fields.Images, fields.ReferenceImages} {
			if len(raw) == 0 {
				continue
			}
			var entries []json.RawMessage
			if raw[0] != '[' {
				entries = []json.RawMessage{raw}
			} else if err := json.Unmarshal(raw, &entries); err != nil {
				return req, nil, err
			}
			if len(images)+len(entries) > maxAttachments {
				return req, nil, errInvalidAttachment
			}
			for _, part := range entries {
				item, err := parseImageInput(part)
				if err != nil {
					return req, nil, err
				}
				images = append(images, item)
			}
		}
	}
	req.Model = strings.ToLower(strings.TrimSpace(req.Model))
	req.Prompt = strings.TrimSpace(req.Prompt)
	// Most OpenAI SDKs send the same model to generation and edit.
	if model == ImageEditModel && req.Model == ImageModel {
		// Keep the requested public id for the API key model scope.
	} else if req.Model != model {
		return req, nil, errInvalidAttachment
	}
	if req.Prompt == "" || len(req.Prompt) > 16<<10 || req.Count < 1 || req.Count > maxAttachments ||
		(model == VideoModel && (req.Count != 1 || req.Format != "url")) ||
		(req.Format != "url" && req.Format != "b64_json") ||
		(model == ImageEditModel && len(images) == 0) || (model == ImageModel && len(images) != 0) {
		return req, nil, errInvalidAttachment
	}
	switch req.Ratio {
	case "", "auto":
		req.Ratio = ""
	case "1024x1024":
		req.Ratio = "1:1"
	case "1536x1024", "1792x1024":
		req.Ratio = "16:9"
	case "1024x1536", "1024x1792":
		req.Ratio = "9:16"
	case "1:1", "16:9", "9:16", "4:3", "3:4":
	default:
		return req, nil, errInvalidAttachment
	}
	if model == VideoModel && (req.Ratio == "4:3" || req.Ratio == "3:4") {
		return req, nil, errInvalidAttachment
	}
	return req, images, nil
}

func namedImageInput(value string) imageInput {
	if strings.HasPrefix(value, "file_db_") {
		return imageInput{fileID: value}
	}
	return imageInput{raw: value}
}

func parseImageInput(raw json.RawMessage) (imageInput, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil && text != "" {
		return namedImageInput(text), nil
	}
	var object struct {
		FileID   string `json:"file_id"`
		URL      string `json:"url"`
		ImageURL string `json:"image_url"`
		Base64   string `json:"b64_json"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return imageInput{}, err
	}
	input := imageInput{raw: first(object.URL, object.ImageURL, object.Base64), fileID: object.FileID}
	if (input.raw == "") == (input.fileID == "") {
		return imageInput{}, errInvalidAttachment
	}
	return input, nil
}

func safeAttachmentName(value string) string {
	value = path.Base(strings.ReplaceAll(value, "\\", "/"))
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	if value == "" || value == "." || value == ".." || value == "/" || len(value) > 255 {
		return "attachment.bin"
	}
	return value
}
