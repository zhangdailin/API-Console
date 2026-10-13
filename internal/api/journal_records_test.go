package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/pricing"
	"orchids-api/internal/testutil"

	"github.com/redis/go-redis/v9"
)

// seedJournal writes raw journal entries into the audit stream in the order the
// real writer uses: an upstream attempt is written BEFORE the request it belongs
// to, and the stream is read newest-first.
func seedJournal(t *testing.T, a *API, events []audit.Event) {
	t.Helper()
	client := a.store.RedisClient()
	key := a.store.RedisPrefix() + "audit:log"
	for _, event := range events {
		if event.Timestamp.IsZero() {
			event.Timestamp = time.Now()
		}
		raw, err := json.Marshal(event)
		testutil.NoError(t, err, "marshal event: %v")
		if _, err := client.XAdd(context.Background(), &redis.XAddArgs{
			Stream: key,
			Values: map[string]interface{}{"data": string(raw), "action": event.Action, "status": event.Status, "kind": string(event.Kind)},
		}).Result(); err != nil {
			t.Fatalf("XAdd: %v", err)
		}
	}
}

func journalRequest(t *testing.T, a *API, query string) map[string]interface{} {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/journal/records"+query, nil)
	recorder := httptest.NewRecorder()
	a.HandleJournalRecords(recorder, request)
	testutil.Equal(t, recorder.Code, http.StatusOK)
	var payload map[string]interface{}
	err := json.Unmarshal(recorder.Body.Bytes(), &payload)
	testutil.CheckNoError(t, err)
	return payload
}

// TestJournalRecords_AttachAttemptsToTheirRequest pins the reported defect that
// every detail panel came back empty. Attempts are written before their request,
// so a reverse-order scan sees them AFTER it; attaching them in a single pass
// could never find them.
func TestJournalRecords_AttachAttemptsToTheirRequest(t *testing.T) {
	s, _ := newTestStore(t, "journal-attempts:")
	t.Cleanup(func() { _ = s.Close() })
	a := &API{store: s}

	seedJournal(t, a, []audit.Event{
		{Kind: audit.KindRequest, Action: "grok_upstream_attempt", RequestID: "req-1", Status: "error", Channel: "grok"},
		{Kind: audit.KindRequest, Action: "grok_upstream_attempt", RequestID: "req-1", Status: "success", Channel: "grok"},
		{Kind: audit.KindRequest, Action: "grok_request", RequestID: "req-1", Status: "ok", Channel: "grok", Model: "grok-4.6"},
	})

	payload := journalRequest(t, a, "?kind=request")
	rows, ok := payload["data"].([]interface{})
	testutil.True(t, ok, "data = %T, want a list")
	testutil.Equal(t, len(rows), 1)
	row, _ := rows[0].(map[string]interface{})
	attempts, _ := row["attempts"].([]interface{})
	testutil.Equal(t, len(attempts), 2)
	event, _ := row["event"].(map[string]interface{})
	testutil.Equal(t, event["request_id"], "req-1")
}

// TestJournalRecords_CursorAdvancesPastScannedEntries pins the second half of the
// defect: returning the last MATCHING record as the cursor made older pages
// unreachable whenever the window held more non-matching entries than one page —
// with a full page of non-matching entries the cursor came back empty and the
// page holding the record could not be requested at all.
func TestJournalRecords_CursorAdvancesPastScannedEntries(t *testing.T) {
	s, _ := newTestStore(t, "journal-cursor:")
	t.Cleanup(func() { _ = s.Close() })
	a := &API{store: s}

	// The one request this test looks for is the OLDEST entry, buried under 40
	// newer operations: exactly the "earlier operation logs cannot be found" case.
	events := []audit.Event{{Kind: audit.KindRequest, Action: "grok_request", RequestID: "req-old", Status: "ok", Channel: "grok"}}
	for i := 0; i < 40; i++ {
		events = append(events, audit.Event{Kind: audit.KindOperation, Action: "config_update", Status: "ok", Details: "tweak"})
	}
	seedJournal(t, a, events)

	// limit=1 makes the scan window (limit*6) smaller than the 41 stored entries.
	first := journalRequest(t, a, "?kind=request&limit=1")
	rows, _ := first["data"].([]interface{})
	testutil.Falsef(t, len(rows) != 0, "first page rows = %d, want 0 (newest 6 entries are operations)", len(rows))
	// The cursor must point past everything that was scanned, so following it
	// eventually reaches the request the first page could not fit.
	cursor, _ := first["next_cursor"].(string)
	testutil.NotEqual(t, cursor, "")
	if scanned, _ := first["scanned"].(float64); scanned <= 0 {
		t.Fatalf("scanned = %v, want the number of entries the page examined", first["scanned"])
	}
	if matched, _ := first["matched"].(float64); matched != 0 {
		t.Fatalf("matched = %v, want 0 on a page of operations", first["matched"])
	}

	found := false
	seen := map[string]bool{}
	for page := 0; page < 12 && cursor != "" && !found; page++ {
		testutil.Falsef(t, seen[cursor], "page %d reused cursor %s: paging is stuck", page, cursor)
		seen[cursor] = true
		payload := journalRequest(t, a, "?kind=request&limit=1&before="+cursor)
		rows, _ := payload["data"].([]interface{})
		for _, raw := range rows {
			row, _ := raw.(map[string]interface{})
			event, _ := row["event"].(map[string]interface{})
			if event["request_id"] == "req-old" {
				found = true
			}
		}
		cursor, _ = payload["next_cursor"].(string)
	}
	testutil.True(t, found, "following next_cursor never reached the buried request: older journal entries are unreachable")
}

// A priced journal row carries the components behind its cost, so the log centre
// can answer "which rate is this" without a second lookup.
func TestJournalRowCarriesItsPricingBreakdown(t *testing.T) {
	breakdown, ok := pricingBreakdownForJournal(audit.Event{
		Action: "grok_request", Model: "grok-4.6", PricingModel: "grok-4.6", PricingVersion: pricing.Version,
		InputTokens: 1000, CachedInputTokens: 400, OutputTokens: 500, CostInUSDTicks: 55_000_000,
	})
	testutil.True(t, ok, "a priced row produced no breakdown")
	testutil.Equal(t, breakdown.Model, "grok-4.6")
	testutil.Equal(t, len(breakdown.Components), 3)
	var total int64
	for _, component := range breakdown.Components {
		total += component.CostInUSDTicks
	}
	testutil.Equal(t, total, breakdown.CostInUSDTicks)

	// An unpriced row stays without one rather than fabricating components.
	_, ok = pricingBreakdownForJournal(audit.Event{Action: "grok_request", Model: "future-model"})
	testutil.False(t, ok, "an unpriced row grew a breakdown")
}

func TestJournalRecordsRecoversPartialQualityHistory(t *testing.T) {
	s, _ := newTestStore(t, "journal-quality-lookback:")
	t.Cleanup(func() { _ = s.Close() })
	a := &API{store: s}
	events := []audit.Event{{Kind: audit.KindRequest, Action: "grok_quality_degraded", RequestID: "quality-1", Attempt: 1, Status: "degraded", Metadata: map[string]interface{}{"reason": "no_reasoning"}}}
	for i := 0; i < 40; i++ {
		events = append(events, audit.Event{Kind: audit.KindOperation, Action: "config_update", Status: "ok"})
	}
	events = append(events,
		audit.Event{Kind: audit.KindRequest, Action: "grok_quality_degraded", RequestID: "quality-1", Attempt: 2, Status: "degraded", Metadata: map[string]interface{}{"reason": "burst_dump"}},
		audit.Event{Kind: audit.KindRequest, Action: "grok_quality_resolution", RequestID: "quality-1", Status: "retried", Metadata: map[string]interface{}{"withheld_attempts": 2}},
		audit.Event{Kind: audit.KindRequest, Action: "grok_request", RequestID: "quality-1", Channel: "grok", Status: "success", Metadata: map[string]interface{}{"quality_resolution": "retried", "quality_withheld_attempts": 2}},
	)
	seedJournal(t, a, events)
	payload := journalRequest(t, a, "?kind=request&quality=any&limit=1")
	rows := payload["data"].([]interface{})
	testutil.Equal(t, len(rows), 1)
	row := rows[0].(map[string]interface{})
	checks := row["quality"].([]interface{})
	testutil.Equal(t, len(checks), 3)
	testutil.EqualAny(t, checks[0].(map[string]interface{})["attempt"], float64(1))
	testutil.EqualAny(t, checks[1].(map[string]interface{})["attempt"], float64(2))
	testutil.EqualAny(t, checks[2].(map[string]interface{})["action"], "grok_quality_resolution")
	testutil.EqualAny(t, payload["next_cursor"], row["id"])
}

func TestJournalRecordsFiltersQualityResolution(t *testing.T) {
	s, _ := newTestStore(t, "journal-quality-filter:")
	t.Cleanup(func() { _ = s.Close() })
	a := &API{store: s}
	for _, resolution := range []string{"retried", "delivered_degraded", "rejected", "undeliverable"} {
		seedJournal(t, a, []audit.Event{
			{Kind: audit.KindRequest, Action: "grok_quality_resolution", RequestID: resolution, Channel: "grok", Status: resolution},
			{Kind: audit.KindRequest, Action: "grok_request", RequestID: resolution, Channel: "grok", Status: "success", Metadata: map[string]interface{}{"quality_resolution": resolution}},
		})
	}
	seedJournal(t, a, []audit.Event{{Kind: audit.KindRequest, Action: "grok_request", RequestID: "healthy", Channel: "grok", Status: "success"}})
	for _, resolution := range []string{"any", "retried", "delivered_degraded", "rejected", "undeliverable"} {
		t.Run(resolution, func(t *testing.T) {
			payload := journalRequest(t, a, "?kind=request&quality="+resolution)
			rows := payload["data"].([]interface{})
			want := 1
			if resolution == "any" {
				want = 4
			}
			testutil.Equal(t, len(rows), want)
			for _, raw := range rows {
				event := raw.(map[string]interface{})["event"].(map[string]interface{})
				testutil.EqualAny(t, event["action"], "grok_request")
				if resolution != "any" {
					testutil.EqualAny(t, event["request_id"], resolution)
				}
			}
		})
	}
}
