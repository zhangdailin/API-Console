package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

func TestMediaUploadAndImportKeepRouteContracts(t *testing.T) {
	useTempMediaCache(t)
	st, err := store.New(store.Options{RedisAddr: miniredis.RunT(t).Addr()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &Handler{lb: loadbalancer.NewWithCacheTTL(st, time.Second)}

	// Only the magic is needed here: the upload contract sniffs the file
	// type and caches the bytes; it does not decode the image.
	png := []byte("\x89PNG\r\n\x1a\n")
	previous := adminMediaInputFetcher
	adminMediaInputFetcher = func(_ context.Context, url string) ([]byte, string, error) {
		if url != "https://example.com/test.png" {
			t.Fatalf("unexpected fetch: %s", url)
		}
		return png, "image/png", nil
	}
	t.Cleanup(func() { adminMediaInputFetcher = previous })

	reservedBefore := mediaInputReservedBytes
	for _, tc := range []struct {
		name            string
		handle          http.HandlerFunc
		admin, imported bool
	}{
		{"client upload", h.HandleMediaInputs, false, false},
		{"admin upload", h.HandleAdminMediaInputs, true, false},
		{"client import", h.HandleMediaInputImport, false, true},
		{"admin import", h.HandleAdminMediaInputImport, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"url":"https://example.com/test.png"}`))
			if !tc.imported {
				var body bytes.Buffer
				form := multipart.NewWriter(&body)
				part, err := form.CreateFormFile("file", "test.png")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := part.Write(png); err != nil {
					t.Fatal(err)
				}
				if err := form.Close(); err != nil {
					t.Fatal(err)
				}
				r = httptest.NewRequest(http.MethodPost, "/", &body)
				r.Header.Set("Content-Type", form.FormDataContentType())
			}
			const clientOwner = "test-key-owner"
			r = r.WithContext(middleware.WithAPIKeyFingerprint(r.Context(), clientOwner))
			rec := httptest.NewRecorder()
			tc.handle(rec, r)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var result map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			idKey, expiryKey, owner, wrongOwner := "file_id", "expires_at", clientOwner, mediaInputAdminOwner
			if tc.admin {
				result, _ = result["data"].(map[string]any)
				idKey, expiryKey, owner, wrongOwner = "fileId", "expiresAt", mediaInputAdminOwner, clientOwner
			}
			id, _ := result[idKey].(string)
			expiresAt, _ := result[expiryKey].(string)
			at, err := time.Parse(time.RFC3339, expiresAt)
			if err != nil || time.Until(at) < mediaInputTTL-time.Minute {
				t.Fatalf("invalid expiry %q: %v", expiresAt, err)
			}
			input, err := st.GetStoredMediaInput(t.Context(), id, owner)
			if err != nil || input.MIMEType != "image/png" || input.SizeBytes != int64(len(png)) {
				t.Fatalf("invalid stored input: %v, %v", input, err)
			}
			if _, err := st.GetStoredMediaInput(t.Context(), id, wrongOwner); !errors.Is(err, store.ErrNoRows) {
				t.Fatalf("wrong owner lookup: %v", err)
			}
		})
	}
	if mediaInputReservedBytes != reservedBefore {
		t.Fatal("persisted media must not also count as an in-flight reservation")
	}

	for _, tc := range []struct {
		code   string
		handle http.HandlerFunc
	}{
		{"invalid_media_url", h.HandleMediaInputImport},
		{"invalidMediaURL", h.HandleAdminMediaInputImport},
	} {
		rec := httptest.NewRecorder()
		tc.handle(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"url":"ftp://example.com/test.png"}`)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`) {
			t.Fatalf("expected %s: %d %s", tc.code, rec.Code, rec.Body.String())
		}
	}
}
