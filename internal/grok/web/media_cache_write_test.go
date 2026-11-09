package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

func useTempMediaCache(t *testing.T) string {
	t.Helper()
	prev := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = prev })
	return cacheBaseDir
}

// Both cache entrances share one writer: the name is content-addressed, an
// existing file is reused, and no temp file is left behind.
func TestMediaCacheWritersShareAtomicContentAddressedWrite(t *testing.T) {
	base := useTempMediaCache(t)
	h := &Handler{}

	name, err := h.cacheMediaBytes("https://assets.grok.com/a.png", "IMAGE", []byte("png-bytes"), "image/png")
	testutil.NoError(t, err)
	again, err := h.cacheMediaBytes("https://assets.grok.com/a.png", "image", []byte("other"), "image/png")
	testutil.NoError(t, err)
	testutil.Equal(t, again, name)
	got, err := os.ReadFile(filepath.Join(base, "image", name))
	testutil.NoError(t, err)
	testutil.Equal(t, string(got), "png-bytes")

	input, err := h.cacheMediaInputBytes("in-1", "video", []byte("mp4-bytes"), "video/mp4")
	testutil.NoError(t, err)
	testutil.True(t, strings.HasPrefix(input, mediaInputFilePrefix))
	_, err = os.Stat(filepath.Join(base, "video", input))
	testutil.NoError(t, err)

	for _, dir := range []string{"image", "video"} {
		entries, err := os.ReadDir(filepath.Join(base, dir))
		testutil.NoError(t, err)
		for _, e := range entries {
			testutil.False(t, strings.Contains(e.Name(), ".tmp-"), e.Name())
		}
	}

	_, err = h.cacheMediaBytes("https://assets.grok.com/b.png", "image", nil, "image/png")
	testutil.Error(t, err)
}
