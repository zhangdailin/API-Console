package qoder

import (
	"testing"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestQoderCapabilitiesUseAccountCatalog(t *testing.T) {
	acc := &store.Account{QoderModelIDs: []string{
		`{"key":"qfmodel","display_name":"Qwen3.8-Flash","enable":true}`,
		`{"key":"disabled-model","name":"Disabled","enable":false}`,
	}}
	for _, model := range []string{"qfmodel", "QWEN3.8-FLASH"} {
		testutil.True(t, (capabilities{}).SupportsModel(acc, model))
	}
	for _, model := range []string{"qwen3.8-max", "disabled-model"} {
		testutil.False(t, (capabilities{}).SupportsModel(acc, model))
	}
	testutil.False(t, (capabilities{}).SupportsModel(&store.Account{}, "qwen3.8-max"))
	testutil.False(t, (capabilities{}).SupportsModel(nil, "qwen3.8-max"))
}
