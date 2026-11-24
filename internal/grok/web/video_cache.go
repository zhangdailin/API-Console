package web

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"orchids-api/internal/grok/grokbase"
)

func videoContentName(path string) string {
	if path == "" {
		return ""
	}
	name := filepath.Base(path)
	if name != sanitizeCachedFilename(name) {
		return ""
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".webm", ".mov", ".m4v":
		return name
	}
	return ""
}

var videoCacheRestores singleflight.Group

func videoOrigin(job *videoJob) string {
	link := strings.TrimSpace(job.VideoURL)
	if strings.HasPrefix(link, "/") && !strings.HasPrefix(link, "//") && (job.Provider == "web" || job.Provider == "") {
		link = defaultAssetsBaseURL + link
	}
	if grokbase.TrustedVideoURL(link) {
		return link
	}
	return ""
}

func videoCacheStem(source string) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(source)))
	return hex.EncodeToString(sum[:])
}

func (h *Handler) openVideoJobFile(ctx context.Context, job *videoJob) (*os.File, error) {
	if path := job.ContentPath; path != "" && validPersistedVideoContentPath(path) {
		file, err := os.Open(path)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	link := videoOrigin(job)
	if link == "" {
		return nil, os.ErrNotExist
	}
	stem := videoCacheStem(link)
	for _, ext := range []string{".mp4", ".webm", ".mov", ".m4v"} {
		path := filepath.Join(cacheBaseDir, "video", stem+ext)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			return os.Open(path)
		}
	}
	result := videoCacheRestores.DoChan(job.OwnerHash+"\x00"+job.ID, func() (any, error) {
		fetch, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		client := h.webClient()
		if client == nil {
			return nil, os.ErrNotExist
		}
		cookie := ""
		if job.AccountID != 0 && job.Provider == "web" && client.shouldSendAuthForAssetURL(link) &&
			h.lb != nil && h.lb.Store != nil {
			if account, err := h.lb.Store.GetAccount(fetch, job.AccountID); err == nil && account.IsGrokWeb() {
				cookie = client.assetDownloadHeaders(SSOToken(account), link).Get("Cookie")
			}
		}
		body, mime, err := grokbase.OpenVideoWithOriginCookie(fetch, client.clientForAsset(true), link, cookie)
		if err != nil {
			return nil, err
		}
		name, err := cacheVideoStream(link, body, mime)
		if err != nil {
			return nil, err
		}
		return filepath.Join(cacheBaseDir, "video", name), nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case got := <-result:
		if got.Err != nil {
			return nil, got.Err
		}
		return os.Open(got.Val.(string))
	}
}

// cacheVideoStream owns the body and exposes the file only after a complete,
// bounded copy and an atomic rename in the same directory.
func cacheVideoStream(source string, body io.ReadCloser, mimeType string) (string, error) {
	defer body.Close()
	name := videoCacheStem(source) + mediaExtFromMime("video", mimeType, source)
	dir := filepath.Join(cacheBaseDir, "video")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	target := filepath.Join(dir, name)
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return name, nil
	}
	if err := writeVideoStream(target, body, grokbase.MaxVideoAssetBytes); err != nil {
		return "", err
	}
	return name, nil
}

func writeVideoStream(target string, body io.Reader, limit int64) error {
	temp, err := os.CreateTemp(filepath.Dir(target), ".video-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	size, err := io.Copy(temp, io.LimitReader(body, limit+1))
	if err == nil {
		if size == 0 || size > limit {
			err = errors.New("video is empty or exceeds the size limit")
		} else {
			err = temp.Chmod(0o644)
		}
	}
	if closeErr := temp.Close(); err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return os.Rename(temp.Name(), target)
}
