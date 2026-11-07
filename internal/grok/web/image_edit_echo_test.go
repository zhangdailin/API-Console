package web

import (
	"testing"

	"orchids-api/internal/testutil"
)

func TestImageEditResultsDropTheEchoedInputs(t *testing.T) {
	payload := map[string]any{"mediaGenInput": map[string]any{
		"imageToImage": map[string]any{"inputAssets": []any{"93826fa7-8737-4ed0-b85e-e27442224814"}},
	}}
	inputs := imageEditInputAssets(payload)
	testutil.Equal(t, len(inputs), 1)
	urls := withoutInputAssets([]string{
		// Captured from a production edit stream that produced no image.
		"https://assets.grok.com/93826fa7-8737-4ed0-b85e-e27442224814/content",
		"https://assets.grok.com/users/u-1/generated/abc/image.jpg",
	}, inputs)
	testutil.Equal(t, len(urls), 1)
	testutil.MustContain(t, urls[0], "/generated/abc/")
}

func TestMediaCreditsExhaustedIsAnImageLimit(t *testing.T) {
	line := map[string]any{"result": map[string]any{"response": map[string]any{
		"error": map[string]any{"message": "You've used up your media generation credits.", "usagePoolExhausted": map[string]any{}},
	}}}
	testutil.True(t, isAppChatImageLimitResponse(line))
}
