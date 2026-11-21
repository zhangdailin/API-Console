package web

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"orchids-api/internal/grok/grokbase"
)

// cacheVideoStream owns the body and exposes the file only after a complete,
// bounded copy and an atomic rename in the same directory.
func cacheVideoStream(source string, body io.ReadCloser, mimeType string) (string, error) {
	defer body.Close()
	sum := sha1.Sum([]byte(strings.TrimSpace(source)))
	name := hex.EncodeToString(sum[:]) + mediaExtFromMime("video", mimeType, source)
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
