package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/channel"
	"strings"
	"testing"
)

// Counting needs neither a model catalog nor configured upstream clients.
// The provider path determines the profile even for an unknown model.
func TestHandleCountTokensUsesProviderPathWithoutCatalog(t *testing.T) {
	h := &Handler{}
	body := `{"model":"unknown","messages":[{"role":"user","content":"hello there, count my tokens"}]}`
	var expectedTokens int
	for _, provider := range channel.All() {
		for _, base := range channel.PrefixesFor(provider.ID) {
			t.Run(base, func(t *testing.T) {
				w := httptest.NewRecorder()
				h.HandleCountTokens(w, httptest.NewRequest(http.MethodPost, base+"/messages/count_tokens", strings.NewReader(body)))
				if w.Code != http.StatusOK {
					t.Fatalf("count = %d %s", w.Code, w.Body.String())
				}
				var result struct {
					InputTokens int    `json:"input_tokens"`
					Profile     string `json:"prompt_profile"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Profile != string(provider.ID) || result.InputTokens <= 0 {
					t.Fatalf("incorrect count/profile: %#v", result)
				}
				if expectedTokens == 0 {
					expectedTokens = result.InputTokens
				}
				if result.InputTokens != expectedTokens {
					t.Fatalf("same input estimated differently: %d != %d", result.InputTokens, expectedTokens)
				}
			})
		}
	}
}
