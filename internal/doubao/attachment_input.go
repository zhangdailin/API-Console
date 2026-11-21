package doubao

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"orchids-api/internal/config"
	"orchids-api/internal/httpclient"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

const maxAttachments = 4

var errInvalidAttachment = errors.New("invalid or unavailable Doubao attachment")

func loadFile(ctx context.Context, s *store.Store, id string) (*store.DoubaoFile, error) {
	owner := middleware.APIKeyFingerprint(ctx)
	id = strings.TrimSpace(id)
	if owner == "" || !strings.HasPrefix(id, "file_db_") || len(id) != len("file_db_")+32 {
		return nil, errInvalidAttachment
	}
	file, err := s.GetDoubaoFile(ctx, owner, id)
	if errors.Is(err, store.ErrNoRows) {
		return nil, errInvalidAttachment
	}
	return file, err
}

func imageNameFromBytes(data []byte) (string, bool) {
	switch http.DetectContentType(data) {
	case "image/png":
		return "image.png", true
	case "image/jpeg":
		return "image.jpg", true
	case "image/gif":
		return "image.gif", true
	case "image/webp":
		return "image.webp", true
	default:
		return "", false
	}
}

func isRemoteAttachment(raw string) bool {
	return strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://")
}

// fetchAttachment sends no provider credentials and uses the same outbound
// guard for caller URLs and for upstream image URLs requested as base64.
func fetchAttachment(ctx context.Context, cfg *config.Config, raw string, limit int) ([]byte, string, error) {
	proxy := httpclient.ProxyFuncFromConfig(cfg)
	u, err := httpclient.CheckRemoteFetchTarget(ctx, raw, proxy != nil)
	if err != nil {
		return nil, "", errInvalidAttachment
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", errInvalidAttachment
	}
	response, err := httpclient.NewRemoteFetchClient(0, proxy).Do(req)
	if err != nil {
		return nil, "", errInvalidAttachment
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", errInvalidAttachment
	}
	if response.ContentLength > int64(limit) {
		return nil, "", &http.MaxBytesError{Limit: int64(limit)}
	}
	data, err := readAttachment(response.Body, limit)
	if err != nil {
		return nil, "", err
	}
	return data, path.Base(u.Path), nil
}

func readAttachment(r io.Reader, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, &http.MaxBytesError{Limit: int64(limit)}
	}
	data, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: could not read payload", errInvalidAttachment)
	}
	if len(data) > limit {
		return nil, &http.MaxBytesError{Limit: int64(limit)}
	}
	if len(data) == 0 {
		return nil, errInvalidAttachment
	}
	return data, nil
}
