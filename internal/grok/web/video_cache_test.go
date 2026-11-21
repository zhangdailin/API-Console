package web

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteVideoStreamPublishesOnlyCompleteBoundedFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "video.mp4")
	for _, fixture := range []struct {
		name string
		body io.Reader
		fail bool
	}{
		{"too large", strings.NewReader("12345"), true},
		{"empty", strings.NewReader(""), true},
		{"interrupted", io.MultiReader(strings.NewReader("123"), failedVideoStream{}), true},
		{"complete", strings.NewReader("1234"), false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			err := writeVideoStream(target, fixture.body, 4)
			if (err != nil) != fixture.fail {
				t.Fatalf("writeVideoStream: %v", err)
			}
			files, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.fail {
				if len(files) != 0 {
					t.Fatalf("partial files remain: %v", files)
				}
				return
			}
			content, err := os.ReadFile(target)
			if len(files) != 1 || err != nil || string(content) != "1234" {
				t.Fatalf("invalid published file: %q, %v", content, err)
			}
		})
	}
}

type failedVideoStream struct{}

func (failedVideoStream) Read([]byte) (int, error) {
	return 0, errors.New("video transfer interrupted")
}
