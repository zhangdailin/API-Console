package grokbase

import (
	"testing"

	"orchids-api/internal/testutil"
)

func TestModelValidationCacheRemembersValidatedModels(t *testing.T) {
	var cache ModelValidationCache
	testutil.False(t, cache.Has("grok-4"), "an empty cache knows no model")
	cache.Remember("grok-4")
	testutil.True(t, cache.Has("grok-4"), "a remembered model is cached")
	testutil.False(t, cache.Has("grok-3"), "another model is not")
	cache.Remember("  ")
	testutil.False(t, cache.Has("  "), "a blank id is never cached")
}
