package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"orchids-api/internal/audit"
	"orchids-api/internal/testutil"
)

// requestEvent builds an inference record the way the request middleware writes it:
// the HTTP status lives in metadata, not in a column of its own.
func requestEvent(id string, status string, httpStatus int) audit.Event {
	event := audit.Event{
		Kind:      audit.KindRequest,
		Action:    "grok_request",
		RequestID: id,
		Channel:   "grok",
		Model:     "grok-4.6",
		Status:    status,
		Timestamp: time.Now(),
	}
	if httpStatus != 0 {
		event.Metadata = map[string]any{"http_status": float64(httpStatus)}
	}
	return event
}

// seedJournalAt writes entries whose stream ids follow their timestamps, the way the
// real writer does. A time filter bounds the scan by stream id, so a fixture with
// wall-clock ids would hide exactly the behaviour under test.
func seedJournalAt(t *testing.T, a *API, events []audit.Event) {
	t.Helper()
	client := a.store.RedisClient()
	key := a.store.RedisPrefix() + "audit:log"
	sequence := 0
	for _, event := range events {
		if event.Timestamp.IsZero() {
			event.Timestamp = time.Now()
		}
		raw, err := json.Marshal(event)
		testutil.NoError(t, err, "marshal event: %v")
		id := fmt.Sprintf("%d-%d", event.Timestamp.UnixMilli(), sequence)
		sequence++
		if _, err := client.XAdd(context.Background(), &redis.XAddArgs{
			Stream: key,
			ID:     id,
			Values: map[string]any{"data": string(raw), "action": event.Action, "status": event.Status, "kind": string(event.Kind)},
		}).Result(); err != nil {
			t.Fatalf("XAdd: %v", err)
		}
	}
}

// TestJournalRecordsPagingReturnsEveryRecord pins the reported defect that a full
// page threw away everything between its last row and the end of the scan window:
// with 120 stored requests and a page size of 50 the list stopped at 50, and with
// 600 it stopped at 100.
func TestJournalRecordsPagingReturnsEveryRecord(t *testing.T) {
	s, _ := newTestStore(t, "journal-paging:")
	t.Cleanup(func() { _ = s.Close() })
	a := &API{store: s}

	const total = 120
	events := make([]audit.Event, 0, total)
	for i := range total {
		events = append(events, requestEvent(fmt.Sprintf("req-%03d", i), "success", 200))
	}
	seedJournal(t, a, events)

	seen := map[string]int{}
	cursor := ""
	for range 10 {
		query := "?kind=request&limit=50"
		if cursor != "" {
			query += "&before=" + cursor
		}
		payload := journalRequest(t, a, query)
		rows, _ := payload["data"].([]any)
		for _, raw := range rows {
			row, _ := raw.(map[string]any)
			event, _ := row["event"].(map[string]any)
			seen[fmt.Sprint(event["request_id"])]++
		}
		cursor, _ = payload["next_cursor"].(string)
		if cursor == "" {
			break
		}
	}

	testutil.Equal(t, len(seen), total)
	for _, count := range seen {
		testutil.Equal(t, count, 1)
	}
}

// TestJournalRecordsAppliesTimeAndOutcomeFilters pins the second reported defect:
// since/until and outcome were never parsed, so "最近失败" still listed old and
// successful requests.
func TestJournalRecordsAppliesTimeAndOutcomeFilters(t *testing.T) {
	s, _ := newTestStore(t, "journal-filters:")
	t.Cleanup(func() { _ = s.Close() })
	a := &API{store: s}

	now := time.Now()
	rateLimited := requestEvent("req-429", "error", 429)
	rateLimited.Timestamp = now.Add(-3 * time.Minute)
	ok := requestEvent("req-ok", "success", 200)
	ok.Timestamp = now.Add(-2 * time.Minute)
	upstream := requestEvent("req-500", "error", 502)
	upstream.Timestamp = now.Add(-time.Minute)
	streamErr := requestEvent("req-stream", "stream_error", 200)
	streamErr.Timestamp = now.Add(-30 * time.Second)
	operation := audit.Event{Kind: audit.KindOperation, Action: "config_update", Status: "ok", Timestamp: now}
	seedJournalAt(t, a, []audit.Event{rateLimited, ok, upstream, streamErr, operation})

	ids := func(query string) []string {
		t.Helper()
		payload := journalRequest(t, a, query)
		rows, _ := payload["data"].([]any)
		found := make([]string, 0, len(rows))
		for _, raw := range rows {
			row, _ := raw.(map[string]any)
			event, _ := row["event"].(map[string]any)
			found = append(found, fmt.Sprint(event["request_id"]))
		}
		return found
	}

	got := ids("?kind=request&outcome=rate_limited")
	testutil.Falsef(t, len(got) != 1 || got[0] != "req-429", "outcome=rate_limited listed %v, want only req-429", got)
	got = ids("?kind=request&outcome=failed")
	testutil.Falsef(t, len(got) != 3, "outcome=failed listed %v, want the three failing requests", got)
	got = ids("?kind=request&outcome=success")
	testutil.Falsef(t, len(got) != 1 || got[0] != "req-ok", "outcome=success listed %v, want only req-ok", got)
	boundary := now.Add(-90 * time.Second).UTC().Format(time.RFC3339)
	// since cuts the two oldest records; the operation is not inference traffic.
	got = ids("?kind=request&since=" + boundary)
	testutil.Falsef(t, len(got) != 2, "since=90s ago listed %v, want the two newest requests", got)
	// until bounds the scan itself, so a window that ended in the past must still be
	// listed from the same boundary.
	got = ids("?kind=request&until=" + boundary)
	testutil.Falsef(t, len(got) != 2, "until=90s ago listed %v, want the two oldest requests", got)
	got = ids("?kind=request&since=" + boundary + "&outcome=failed")
	testutil.Falsef(t, len(got) != 2, "since+failing listed %v, want the two newest failing requests", got)

	// The page must state which result class it was narrowed to, or the reader
	// cannot tell a filtered list from an empty one.
	payload := journalRequest(t, a, "?kind=request&outcome=failed&since="+boundary)
	used, _ := payload["filter_used"].(map[string]any)
	testutil.Equal(t, used["outcome_label"], "失败（全部失败类型）")
	testutil.False(t, used["since"] == nil, "filter_used.since is missing: the coverage note cannot warn about the retention edge")
}

// TestJournalRecordsOutcomeClassMatchesOverview pins that each row carries the
// result class the operations overview counts, which the row badge and the
// drill-down both name.
func TestJournalRecordsOutcomeClassMatchesOverview(t *testing.T) {
	s, _ := newTestStore(t, "journal-class:")
	t.Cleanup(func() { _ = s.Close() })
	a := &API{store: s}

	seedJournal(t, a, []audit.Event{
		requestEvent("req-429", "error", 429),
		requestEvent("req-402", "error", 402),
		requestEvent("req-401", "error", 401),
		requestEvent("req-500", "error", 503),
		requestEvent("req-400", "error", 400),
		requestEvent("req-ok", "success", 200),
		requestEvent("req-stop", "stop", 200),
		requestEvent("req-tools", "tool_calls", 200),
		requestEvent("req-length", "length", 200),
		requestEvent("req-filtered", "content_filter", 200),
		requestEvent("req-error-200", "error", 200),
	})

	payload := journalRequest(t, a, "?kind=request&limit=50")
	rows, _ := payload["data"].([]any)
	classes := map[string]string{}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		event, _ := row["event"].(map[string]any)
		classes[fmt.Sprint(event["request_id"])] = fmt.Sprint(row["outcome_class"])
	}
	want := map[string]string{
		"req-429":       "rate_limited",
		"req-402":       "quota_exhausted",
		"req-401":       "upstream_auth",
		"req-500":       "server_error",
		"req-400":       "client_error",
		"req-ok":        "success",
		"req-stop":      "success",
		"req-tools":     "success",
		"req-length":    "success",
		"req-filtered":  "success",
		"req-error-200": "failed",
	}
	for id, class := range want {
		testutil.Equal(t, classes[id], class)
	}
}

// TestJournalRecordsRejectsUnparseableTimeFilter pins that a filter the server
// cannot honour is refused instead of silently ignored.
func TestJournalRecordsRejectsUnparseableTimeFilter(t *testing.T) {
	s, _ := newTestStore(t, "journal-bad-time:")
	t.Cleanup(func() { _ = s.Close() })
	a := &API{store: s}

	request := httptest.NewRequest(http.MethodGet, "/api/journal/records?since=yesterday", nil)
	recorder := httptest.NewRecorder()
	a.HandleJournalRecords(recorder, request)
	testutil.Equal(t, recorder.Code, http.StatusBadRequest)
	testutil.NotEqual(t, recorder.Body.String(), "")
}

// TestJournalRecordsRecoversAttemptsBehindThePageWindow pins the fifth reported
// defect: attempts were only joined from inside the page window, so a request that
// ran while other traffic was journalled came back with an empty detail panel even
// though its attempts were still retained.
func TestJournalRecordsRecoversAttemptsBehindThePageWindow(t *testing.T) {
	s, _ := newTestStore(t, "journal-attempt-lookback:")
	t.Cleanup(func() { _ = s.Close() })
	a := &API{store: s}

	events := []audit.Event{
		{Kind: audit.KindRequest, Action: "grok_upstream_attempt", RequestID: "req-old", Status: "error", Channel: "grok", Attempt: 1, Metadata: map[string]any{"http_status": float64(429)}},
	}
	// Traffic that finished while the request above was still running.
	for range 40 {
		events = append(events, audit.Event{Kind: audit.KindOperation, Action: "config_update", Status: "ok"})
	}
	events = append(events, requestEvent("req-old", "error", 429))
	seedJournal(t, a, events)

	// limit=1 makes the scan window six entries: the attempt stays far outside it.
	payload := journalRequest(t, a, "?kind=request&limit=1")
	rows, _ := payload["data"].([]any)
	testutil.Equal(t, len(rows), 1)
	row, _ := rows[0].(map[string]any)
	attempts, _ := row["attempts"].([]any)
	testutil.Equal(t, len(attempts), 1)
	attempt, _ := attempts[0].(map[string]any)
	testutil.Equal(t, attempt["request_id"], "req-old")

	// The lookback must not move the cursor: the next page still starts before the
	// last row shown, so no record behind it is skipped.
	cursor, _ := payload["next_cursor"].(string)
	testutil.False(t, cursor == "", "next_cursor is empty on a full page")
}
