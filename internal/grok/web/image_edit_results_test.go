package web

import (
	"testing"

	"orchids-api/internal/testutil"
)

func editFrame(url string, progress any, extra map[string]any) map[string]any {
	frame := map[string]any{"imageUrl": url, "imageIndex": 0}
	if progress != nil {
		frame["progress"] = progress
	}
	for k, v := range extra {
		frame[k] = v
	}
	return map[string]any{"streamingImageGenerationResponse": frame}
}

// A grok.com edit stream sends one image several times while it sharpens.
// Only the finished frame is a result; taking two images used to return the
// finished image and a blurry 50% preview.
func TestImageEditKeepsOnlyTheFinishedImage(t *testing.T) {
	const (
		preview1 = "https://assets.grok.com/users/u1/generated/e1/preview-30.jpg"
		preview2 = "https://assets.grok.com/users/u1/generated/e1/preview-60.jpg"
		final    = "https://assets.grok.com/users/u1/generated/e1/image.jpg"
		echoed   = "https://assets.grok.com/users/u1/input-asset-1/content.jpg"
	)
	lines := []map[string]any{
		editFrame(preview1, 30, nil),
		editFrame(preview2, 60.0, nil),
		editFrame(final, 100, nil),
		{"modelResponse": map[string]any{"fileAttachments": []any{echoed}, "generatedImageUrls": []any{final}}},
	}
	var urls []string
	for _, line := range lines {
		urls = appendImageEditResultURLs(urls, line)
	}
	urls = normalizeGeneratedImageURLs(withoutInputAssets(urls, []string{"input-asset-1"}), 2)
	testutil.Equal(t, len(urls), 1)
	testutil.Equal(t, urls[0], final)
}

func TestImageEditPreviewFrame(t *testing.T) {
	testutil.True(t, imageEditPreviewFrame(editFrame("u", 50, nil)), "a 50% frame is a preview")
	testutil.False(t, imageEditPreviewFrame(editFrame("u", 100, nil)), "a 100% frame is the result")
	testutil.False(t, imageEditPreviewFrame(editFrame("u", 40, map[string]any{"isFinal": true})), "isFinal marks the result")
	testutil.True(t, imageEditPreviewFrame(editFrame("u", 100, map[string]any{"moderated": true})), "a moderated image is withheld")
	testutil.False(t, imageEditPreviewFrame(editFrame("u", nil, nil)), "a frame without progress is not assumed to be a preview")
	testutil.False(t, imageEditPreviewFrame(map[string]any{"modelResponse": map[string]any{"message": "x"}}), "other lines are not frames")
}
