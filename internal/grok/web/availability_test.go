package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func availabilityFor(t *testing.T, accounts ...*store.Account) map[string]any {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "avail:"})
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	for _, acc := range accounts {
		testutil.NoError(t, s.CreateAccount(context.Background(), acc))
	}
	h := NewHandler(nil, loadbalancer.NewWithCacheTTL(s, time.Second))
	rec := httptest.NewRecorder()
	h.HandleAvailability(rec, httptest.NewRequest("GET", "/api/grok/availability", nil))
	var out map[string]any
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func planAccount(plan string, imagine store.GrokImagineQuota) *store.Account {
	return &store.Account{AccountType: "grok", GrokProvider: ProviderWeb, CredentialType: "sso", ClientCookie: "sso=x",
		Enabled: true, Subscription: plan, GrokWebQuota: store.GrokWebQuotaSnapshot{Imagine: imagine}}
}

// The quota grok.com reported for a Free account: a small daily Imagine
// allowance, no separate standard image product (null) and no image edits.
func freeQuota(imageProLeft int64) store.GrokImagineQuota {
	return store.GrokImagineQuota{
		Image:     store.GrokMediaWindow{Known: true},
		ImagePro:  store.GrokMediaWindow{Known: true, Available: imageProLeft > 0, Metered: true, Remaining: imageProLeft, ResetAt: time.Now().Add(time.Hour)},
		ImageEdit: store.GrokMediaWindow{Known: true},
	}
}

func TestAvailabilityReportsPlansAndMedia(t *testing.T) {
	out := availabilityFor(t, planAccount("basic", freeQuota(8)), planAccount("basic", freeQuota(0)))
	sameJSON(t, out["web_plans"], any(map[string]any{"free": float64(2), "paid": float64(0), "unknown": float64(0)}))
	sameJSON(t, out["media"], any(map[string]any{"image": true, "image_pro": true, "image_edit": false}))

	spent := availabilityFor(t, planAccount("basic", freeQuota(0)))
	sameJSON(t, spent["media"], any(map[string]any{"image": false, "image_pro": false, "image_edit": false}))

	paid := store.GrokImagineQuota{ImageEdit: store.GrokMediaWindow{Known: true, Available: true, Metered: true, Remaining: 10}}
	mixed := availabilityFor(t, planAccount("basic", freeQuota(0)), planAccount("super", paid))
	sameJSON(t, mixed["web_plans"], any(map[string]any{"free": float64(1), "paid": float64(1), "unknown": float64(0)}))
	sameJSON(t, mixed["media"], any(map[string]any{"image": true, "image_pro": true, "image_edit": true}))
}

func sameJSON(t *testing.T, got, want any) {
	t.Helper()
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	testutil.Equal(t, string(a), string(b))
}
