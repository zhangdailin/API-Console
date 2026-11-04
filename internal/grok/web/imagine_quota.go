package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/store"
)

// Grok Web media is metered per product by /rest/media/imagine/quota_info:
// a Free plan offers no image edits and a small daily Imagine allowance, a
// paid plan meters some products per product and draws the rest from its
// shared weekly pool. Routing a media request to an account without the
// product's allowance only earns an upstream 429, so selection is fenced by
// these windows, as grok2api does.
const imagineQuotaPath = "/rest/media/imagine/quota_info"

// Media products a request can consume.
const (
	mediaNone      = ""
	mediaImagePro  = "image_pro"
	mediaImageEdit = "image_edit"
	mediaVideo     = "video"
	mediaVideo720p = "video_720p"
	// mediaLite is a lite image: a fast chat, metered by the fast quota on a
	// Free plan and by the weekly pool on a paid one.
	mediaLite = "lite"
)

// GetImagineQuota reads the per-product media allowance of an SSO account.
func (c *Client) GetImagineQuota(ctx context.Context, token string) (store.GrokImagineQuota, error) {
	reqURL := c.baseURL() + imagineQuotaPath
	headers := c.headersFor(ctx, token, http.MethodPost, reqURL)
	headers.Set("Referer", c.baseURL()+"/imagine")
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, []byte("{}"), headers, false)
	if err != nil {
		return store.GrokImagineQuota{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return store.GrokImagineQuota{}, err
	}
	return parseImagineQuota(body, time.Now().UTC())
}

func parseImagineQuota(body []byte, now time.Time) (store.GrokImagineQuota, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return store.GrokImagineQuota{}, fmt.Errorf("grok imagine quota: %w", err)
	}
	parse := func(name string) store.GrokMediaWindow {
		raw, ok := fields[name]
		if !ok {
			return store.GrokMediaWindow{}
		}
		window := store.GrokMediaWindow{Known: true}
		if strings.TrimSpace(string(raw)) == "null" {
			return window // the plan does not offer the product
		}
		var product struct {
			Available         *bool      `json:"available"`
			RemainingQueries  *int64     `json:"remainingQueries"`
			WindowSizeSeconds *int64     `json:"windowSizeSeconds"`
			NextAvailableAt   *time.Time `json:"nextAvailableAt"`
		}
		if json.Unmarshal(raw, &product) != nil || product.Available == nil {
			return store.GrokMediaWindow{}
		}
		window.Available = *product.Available
		if product.WindowSizeSeconds != nil {
			window.WindowSeconds = *product.WindowSizeSeconds
		}
		if product.RemainingQueries != nil {
			window.Metered = true
			window.Remaining = max(0, *product.RemainingQueries)
		}
		switch {
		case product.NextAvailableAt != nil:
			window.ResetAt = product.NextAvailableAt.UTC()
		case window.WindowSeconds > 0:
			window.ResetAt = now.Add(time.Duration(window.WindowSeconds) * time.Second)
		}
		return window
	}
	return store.GrokImagineQuota{
		ImagePro:  parse("imagePro"),
		ImageEdit: parse("imageEdit"),
		Video:     parse("video"),
		Video720p: parse("video720p"),
		SyncedAt:  now,
	}, nil
}

// mediaProductFor names the media allowance a request for spec consumes.
// Lite images are a fast chat and draw from the fast quota instead.
func mediaProductFor(spec ModelSpec, videoResolution string) string {
	id := normalizeModelID(spec.ID)
	switch {
	case spec.IsVideo:
		if strings.EqualFold(strings.TrimSpace(videoResolution), "720p") {
			return mediaVideo720p
		}
		return mediaVideo
	case isImageEditModel(id):
		return mediaImageEdit
	case spec.IsImage && id != "grok-imagine-image-lite":
		return mediaImagePro
	case id == "grok-imagine-image-lite":
		return mediaLite
	}
	return mediaNone
}

func mediaWindow(acc *store.Account, product string) (store.GrokMediaWindow, string) {
	q := acc.GrokWebQuota.Imagine
	switch product {
	case mediaImagePro:
		return q.ImagePro, "标准/高质量出图"
	case mediaImageEdit:
		return q.ImageEdit, "改图"
	case mediaVideo:
		return q.Video, "视频"
	case mediaVideo720p:
		return q.Video720p, "720p 视频"
	}
	return store.GrokMediaWindow{}, ""
}

// mediaAllowed reports whether an account still has the allowance a media
// product needs. An account never observed is allowed: its first request is
// what reveals the allowance.
func mediaAllowed(acc *store.Account, product string, now time.Time) bool {
	if acc == nil || product == mediaNone {
		return true
	}
	// A paid plan's media draws from its weekly pool even when a product
	// reports its own counter (an exhausted pool refuses them all).
	if weekly := acc.GrokWebQuota.Weekly; weekly.HasLimit && weekly.Remaining <= 0 && (weekly.ResetAt.IsZero() || now.Before(weekly.ResetAt)) {
		return false
	}
	if product == mediaLite {
		return true // the fast quota is the chat's, fenced by the pool itself
	}
	window, _ := mediaWindow(acc, product)
	if !window.Known {
		return true
	}
	if !window.Available {
		// A product that is unavailable only until its next window reopens is
		// spent, not missing; both refuse the request now.
		return false
	}
	if window.Metered && window.Remaining <= 0 {
		return !window.ResetAt.IsZero() && !now.Before(window.ResetAt)
	}
	return true
}

// mediaUnavailableMessage explains why no account could take a media request.
func (h *Handler) mediaUnavailableMessage(ctx context.Context, product string) string {
	_, label := mediaWindow(&store.Account{}, product)
	if label == "" {
		label = "该媒体功能"
	}
	msg := fmt.Sprintf("当前没有可用于%s的 Grok Web 账号额度", label)
	if product == mediaImageEdit {
		msg += "（Free 账号不提供改图；付费账号的改图额度或周额度已用完）"
	} else {
		msg += "（各账号该项额度或付费周额度已用完，等待重置后再试）"
	}
	return msg
}

// noteMediaExhausted records that an account just ran out of a media
// product, so selection stops sending it the product until the next sync.
// The account itself stays available for everything else.
func (h *Handler) noteMediaExhausted(ctx context.Context, acc *store.Account, product string) {
	if h == nil || h.lb == nil || h.lb.Store == nil || acc == nil || acc.ID == 0 || product == mediaNone {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	latest, err := h.lb.Store.GetAccount(writeCtx, acc.ID)
	if err != nil || latest == nil {
		return
	}
	now := time.Now().UTC()
	if product == mediaLite {
		// A paid plan's lite images draw from its weekly pool; a spent pool
		// refuses every media product until it resets.
		if weekly := &latest.GrokWebQuota.Weekly; weekly.HasLimit {
			weekly.Remaining, weekly.UsagePercent, weekly.HasUsage = 0, 100, true
			latest.GrokWebQuota.SyncedAt = now
			if err := h.lb.Store.UpdateAccount(writeCtx, latest); err == nil {
				*acc = *latest
			}
		}
		return
	}
	q := &latest.GrokWebQuota.Imagine
	var window *store.GrokMediaWindow
	switch product {
	case mediaImagePro:
		window = &q.ImagePro
	case mediaImageEdit:
		window = &q.ImageEdit
	case mediaVideo:
		window = &q.Video
	case mediaVideo720p:
		window = &q.Video720p
	default:
		return
	}
	window.Known, window.Metered, window.Remaining = true, true, 0
	if window.ResetAt.IsZero() || !window.ResetAt.After(now) {
		hold := time.Duration(window.WindowSeconds) * time.Second
		if hold <= 0 {
			hold = time.Hour
		}
		window.ResetAt = now.Add(hold)
	}
	q.SyncedAt = now
	latest.GrokWebQuota.SyncedAt = now
	if err := h.lb.Store.UpdateAccount(writeCtx, latest); err != nil {
		return
	}
	*acc = *latest
}

type mediaProductKey struct{}

// withMediaProduct records the media product a request consumes, so account
// selection can fence it.
func withMediaProduct(ctx context.Context, product string) context.Context {
	return context.WithValue(ctx, mediaProductKey{}, product)
}

func mediaProductFromContext(ctx context.Context) string {
	product, _ := ctx.Value(mediaProductKey{}).(string)
	return product
}
