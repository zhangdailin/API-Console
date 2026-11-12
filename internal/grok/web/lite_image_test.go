package web

import (
	"testing"

	"orchids-api/internal/testutil"
)

// A lite call draws two pictures and only the first is the drawing of the
// prompt. The collector takes the first one per call, in stream order (not
// the URL sort order), and skips a picture an earlier call already returned.
func TestFirstLiteImageTakesTheFirstDrawing(t *testing.T) {
	const (
		drawing   = "https://assets.grok.com/users/u/generated/zzz-first/image.jpg"
		unrelated = "https://assets.grok.com/users/u/generated/aaa-second/image.jpg"
	)
	testutil.Equal(t, firstLiteImage([]string{drawing, unrelated}, nil), drawing)
	testutil.Equal(t, firstLiteImage([]string{drawing, unrelated}, []string{drawing}), unrelated)
	testutil.Equal(t, firstLiteImage([]string{"not a url", drawing}, nil), drawing)
	testutil.Equal(t, firstLiteImage(nil, nil), "")
}
