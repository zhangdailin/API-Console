package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"orchids-api/internal/testutil"
)

// imagineScriptedSocket reads the reset and request messages, then hands the
// request ID to script, which plays the upstream side of one round. When hold
// is true the server keeps the connection open after the script, the way
// grok.com does; otherwise it closes it.
func imagineScriptedSocket(t *testing.T, hold bool, script func(conn *websocket.Conn, requestID string)) *websocket.Conn {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var request struct {
			Item struct {
				Content []struct {
					RequestID string `json:"requestId"`
				} `json:"content"`
			} `json:"item"`
		}
		_ = json.Unmarshal(raw, &request)
		requestID := ""
		if len(request.Item.Content) > 0 {
			requestID = request.Item.Content[0].RequestID
		}
		script(conn, requestID)
		if hold {
			_, _, _ = conn.ReadMessage()
		}
	}))
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func sendImagineFrames(conn *websocket.Conn, frames ...string) {
	for _, frame := range frames {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(frame))
	}
}

// runImagineRoundForTest runs one round and returns every event it emitted.
func runImagineRoundForTest(t *testing.T, conn *websocket.Conn, needed int, timeout time.Duration) (int, []imagineWSEvent, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	events := make(chan imagineWSEvent, 64)
	finals, err := runImagineWSRound(ctx, conn, "a red apple", "1:1", false, false, needed, events)
	close(events)
	var got []imagineWSEvent
	for ev := range events {
		got = append(got, ev)
	}
	return finals, got, err
}

func finalImagineEvents(events []imagineWSEvent) []imagineWSEvent {
	var finals []imagineWSEvent
	for _, ev := range events {
		if ev.Final {
			finals = append(finals, ev)
		}
	}
	return finals
}

// A silent upstream used to be re-read after every read timeout; gorilla
// panics on the 1000th read of a failed connection, outside any recover.
func TestImagineRoundStopsOnSilentUpstreamWithoutRereading(t *testing.T) {
	conn := imagineScriptedSocket(t, true, func(*websocket.Conn, string) {})
	finals, _, err := runImagineRoundForTest(t, conn, 1, 200*time.Millisecond)
	testutil.Equal(t, finals, 0)
	testutil.True(t, errors.Is(err, context.DeadlineExceeded), "a cancelled round reports the context error")

	// The read error is sticky: the round must not have left the connection
	// in a state where it kept reading. One more read fails, it does not panic.
	_, _, readErr := conn.ReadMessage()
	testutil.Error(t, readErr)
}

func TestImagineRoundEndsWhenEveryGenerationIsModerated(t *testing.T) {
	conn := imagineScriptedSocket(t, true, func(conn *websocket.Conn, _ string) {
		sendImagineFrames(conn,
			`{"type":"json","current_status":"start_stage","image_id":"a1"}`,
			`{"type":"json","current_status":"completed","image_id":"a1","moderated":true}`,
		)
	})
	started := time.Now()
	finals, events, err := runImagineRoundForTest(t, conn, 1, 10*time.Second)
	testutil.NoError(t, err)
	testutil.Equal(t, finals, 0)
	testutil.True(t, time.Since(started) < 5*time.Second, "a fully moderated round ends without waiting for a timeout")
	moderated := 0
	for _, ev := range events {
		if ev.Type == "moderated" {
			moderated++
		}
	}
	testutil.Equal(t, moderated, 1)
}

func TestImagineRoundDeliversTheFinalFrameNotThePreview(t *testing.T) {
	conn := imagineScriptedSocket(t, true, func(conn *websocket.Conn, _ string) {
		sendImagineFrames(conn,
			`{"type":"json","current_status":"start_stage","image_id":"a1"}`,
			`{"type":"image","url":"users/u/generated/a1-part-0/image.jpg","image_id":"a1","blob":"cHJldmlldw==","percentage_complete":50}`,
			`{"type":"json","current_status":"completed","image_id":"a1"}`,
			// The final frame arrives after "completed".
			`{"type":"image","url":"users/u/images/a1.jpg","blob":"ZmluYWw=","percentage_complete":100}`,
		)
	})
	finals, events, err := runImagineRoundForTest(t, conn, 1, 10*time.Second)
	testutil.NoError(t, err)
	testutil.Equal(t, finals, 1)
	got := finalImagineEvents(events)
	testutil.Equal(t, len(got), 1)
	testutil.Equal(t, got[0].Blob, "ZmluYWw=")
	testutil.Equal(t, got[0].URL, "users/u/images/a1.jpg")
}

func TestImagineRoundAcceptsTheURLOnTheCompletedFrame(t *testing.T) {
	conn := imagineScriptedSocket(t, true, func(conn *websocket.Conn, _ string) {
		sendImagineFrames(conn,
			`{"type":"image","url":"users/u/images/a1.jpg","blob":"cHJldmlldw==","percentage_complete":40}`,
			`{"type":"json","current_status":"completed","image_id":"a1","url":"users/u/images/a1.jpg"}`,
		)
	})
	finals, events, err := runImagineRoundForTest(t, conn, 1, 10*time.Second)
	testutil.NoError(t, err)
	testutil.Equal(t, finals, 1)
	got := finalImagineEvents(events)
	testutil.Equal(t, len(got), 1)
	testutil.Equal(t, got[0].URL, "users/u/images/a1.jpg")
	testutil.Equal(t, got[0].Blob, "")
}

// No start_stage frame, and an asset URL without an "/images/" segment: the
// slot is created from the frame itself and keyed by the last path segment.
func TestImagineRoundIdentifiesSlotsWithoutStartStage(t *testing.T) {
	conn := imagineScriptedSocket(t, true, func(conn *websocket.Conn, _ string) {
		sendImagineFrames(conn,
			`{"type":"image","url":"https://assets.grok.com/users/u/generated/b2.png?x=1","blob":"ZmluYWw=","percentage_complete":100}`,
			`{"type":"json","current_status":"completed","job_id":"b2"}`,
		)
	})
	finals, events, err := runImagineRoundForTest(t, conn, 1, 10*time.Second)
	testutil.NoError(t, err)
	testutil.Equal(t, finals, 1)
	got := finalImagineEvents(events)
	testutil.Equal(t, len(got), 1)
	testutil.Equal(t, got[0].ImageID, "b2")
	testutil.Equal(t, got[0].Blob, "ZmluYWw=")
}

func TestImagineRoundIgnoresFramesOfAnotherRequest(t *testing.T) {
	conn := imagineScriptedSocket(t, true, func(conn *websocket.Conn, requestID string) {
		sendImagineFrames(conn,
			`{"type":"image","requestId":"someone-else","url":"users/u/images/zz.jpg","blob":"c3RhbGU=","percentage_complete":100}`,
			`{"type":"json","requestId":"someone-else","current_status":"completed","image_id":"zz"}`,
			`{"type":"image","requestId":"`+requestID+`","url":"users/u/images/a1.jpg","blob":"ZmluYWw=","percentage_complete":100}`,
			`{"type":"json","requestId":"`+requestID+`","current_status":"completed","image_id":"a1"}`,
		)
	})
	finals, events, err := runImagineRoundForTest(t, conn, 1, 10*time.Second)
	testutil.NoError(t, err)
	testutil.Equal(t, finals, 1)
	got := finalImagineEvents(events)
	testutil.Equal(t, len(got), 1)
	testutil.Equal(t, got[0].ImageID, "a1")
}

// Two generations, one moderated: the round ends with one image instead of
// waiting for a second that will never come.
func TestImagineRoundCountsModeratedGenerationsAsTerminal(t *testing.T) {
	conn := imagineScriptedSocket(t, true, func(conn *websocket.Conn, _ string) {
		sendImagineFrames(conn,
			`{"type":"image","url":"users/u/images/a1.jpg","blob":"ZmluYWw=","percentage_complete":100}`,
			`{"type":"json","current_status":"completed","image_id":"a1"}`,
			`{"type":"json","current_status":"completed","image_id":"a2","moderated":true}`,
		)
	})
	started := time.Now()
	finals, _, err := runImagineRoundForTest(t, conn, 2, 10*time.Second)
	testutil.NoError(t, err)
	testutil.Equal(t, finals, 1)
	testutil.True(t, time.Since(started) < 5*time.Second, "the round ends once both generations are terminal")
}

// Upstream said "completed" and hung up without the final frame: the last
// preview is the best available result and is delivered once.
func TestImagineRoundFallsBackToThePreviewWhenTheSocketCloses(t *testing.T) {
	conn := imagineScriptedSocket(t, false, func(conn *websocket.Conn, _ string) {
		sendImagineFrames(conn,
			`{"type":"image","url":"users/u/images/a1.jpg","blob":"cHJldmlldw==","percentage_complete":80}`,
			`{"type":"json","current_status":"completed","image_id":"a1"}`,
		)
	})
	finals, events, err := runImagineRoundForTest(t, conn, 1, 10*time.Second)
	testutil.NoError(t, err)
	testutil.Equal(t, finals, 1)
	got := finalImagineEvents(events)
	testutil.Equal(t, len(got), 1)
	testutil.Equal(t, got[0].Blob, "cHJldmlldw==")
}

// A preview of a generation that never completed is not a result.
func TestImagineRoundDoesNotDeliverAnUnfinishedPreview(t *testing.T) {
	conn := imagineScriptedSocket(t, false, func(conn *websocket.Conn, _ string) {
		sendImagineFrames(conn,
			`{"type":"image","url":"users/u/images/a1.jpg","blob":"cHJldmlldw==","percentage_complete":30}`,
		)
	})
	finals, events, err := runImagineRoundForTest(t, conn, 1, 10*time.Second)
	testutil.Error(t, err)
	testutil.Equal(t, finals, 0)
	testutil.Equal(t, len(finalImagineEvents(events)), 0)
}

func TestParseImagineWSImageID(t *testing.T) {
	testutil.Equal(t, parseImagineWSImageID("users/u/images/a1.jpg"), "a1")
	testutil.Equal(t, parseImagineWSImageID("https://assets.grok.com/users/u/images/a1/content?x=1"), "a1")
	testutil.Equal(t, parseImagineWSImageID("https://assets.grok.com/users/u/generated/b2.png#frag"), "b2")
	testutil.Equal(t, parseImagineWSImageID(""), "")
}

func TestImagineIncompleteErrorMessage(t *testing.T) {
	testutil.Equal(t, (&imagineIncompleteError{got: 0, want: 2}).Error(), "no image generated")
	testutil.Equal(t, (&imagineIncompleteError{got: 1, want: 2}).Error(), "upstream returned only 1/2 usable images")
}

// A JSON edit request's size used to be read from the (empty) form and so
// skipped validation.
func TestImageEditJSONSizeIsValidated(t *testing.T) {
	body := `{"model":"grok-imagine-image-edit","prompt":"make it blue","size":"999x999","image":{"url":"https://example.com/a.png"}}`
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/images/edits", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	(&Handler{}).HandleImagesEdits(rec, req)
	testutil.Equal(t, rec.Code, http.StatusBadRequest)
	testutil.MustContain(t, rec.Body.String(), "image edit size must be")
}
