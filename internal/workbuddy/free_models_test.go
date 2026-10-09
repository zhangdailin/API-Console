package workbuddy

import (
	"orchids-api/internal/testutil"
	"testing"
)

func TestIsFreeModelUsesConfirmedSetOnly(t *testing.T) {
	for _, id := range []string{"hy3", " hy4-preview-f ", "DEEPSEEK-V4.1-FLASH", "glm-5.3-flash"} {
		testutil.CheckTrue(t, IsFreeModel(id), "IsFreeModel(%q)=false")
	}
	for _, id := range []string{"default-model", "gemini-3.5-flash", "glm-5.3", "unknown"} {
		testutil.CheckFalsef(t, IsFreeModel(id), "IsFreeModel(%q)=true, must not infer free from its name", id)
	}
}

func TestIsFreeModelInCatalogRequiresAccountAdvertisement(t *testing.T) {
	ids := []string{`{"id":"hy3","name":"HY3"}`, `{"id":"deepseek-v4.1-flash"}`, `{"id":"gpt-5.6-sol"}`}
	testutil.False(t, !IsFreeModelInCatalog(ids, "hy3") || !IsFreeModelInCatalog(ids, "deepseek-v4.1-flash"), "confirmed advertised free models should pass")
	testutil.False(t, IsFreeModelInCatalog(ids, "hy4-preview-f"), "free model missing from this account catalog must not pass")
	testutil.False(t, IsFreeModelInCatalog(ids, "gpt-5.6-sol"), "advertised paid model must not pass")
}

func TestBareCatalogDoesNotGrantFreeEntitlement(t *testing.T) {
	if IsFreeModelInCatalog([]string{"hy3"}, "hy3") {
		t.Fatal("retired bare snapshot granted entitlement")
	}
	if !IsFreeModelInCatalog(CatalogSnapshot([]WorkBuddyModel{{ID: "hy3"}}), "hy3") {
		t.Fatal("current refreshed snapshot denied")
	}
}
