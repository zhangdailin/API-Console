package grok

import (
	"errors"
	"fmt"
	"io"
	"orchids-api/internal/testutil"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildSemanticIdleIgnoresNonGeneratedEvents(t *testing.T) {
	tests := []struct {
		name  string
		chunk string
	}{
		{name: "keepalive comment", chunk: ": keep-alive\n\n"},
		{name: "doom loop control", chunk: "event: response.doom_loop_check\ndata: {\"type\":\"response.doom_loop_check\"}\n\n"},
		{name: "lifecycle event", chunk: "event: response.in_progress\ndata: {\"type\":\"response.in_progress\"}\n\n"},
		{name: "empty generated delta", chunk: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"\"}\n\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader, writer := io.Pipe()
			body := wrapBuildSemanticIdle(reader, 60*time.Millisecond)
			readDone := make(chan error, 1)
			go func() {
				_, err := io.Copy(io.Discard, body)
				readDone <- err
			}()
			writeDone := make(chan struct{})
			go func() {
				defer close(writeDone)
				defer writer.Close()
				for {
					if _, err := io.WriteString(writer, test.chunk); err != nil {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
			}()

			select {
			case err := <-readDone:
				testutil.Falsef(t, !errors.Is(err, errGrokSemanticIdle), "body read error = %v, want ErrUpstreamStreamIdleTimeout", err)
			case <-time.After(time.Second):
				t.Fatal("semantic idle timeout did not interrupt the stream")
			}
			<-writeDone
		})
	}
}

func TestBuildSemanticIdleResetsOnGeneratedDelta(t *testing.T) {
	reader, writer := io.Pipe()
	body := wrapBuildSemanticIdle(reader, 300*time.Millisecond)
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		readDone <- err
	}()

	chunk := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"
	_, err := io.WriteString(writer, chunk)
	testutil.NoError(t, err)
	time.Sleep(180 * time.Millisecond)
	_, err = io.WriteString(writer, chunk)
	testutil.NoError(t, err)
	time.Sleep(180 * time.Millisecond)
	testutil.NoError(t, writer.Close())

	select {
	case err := <-readDone:
		testutil.NoError(t, err, "body read error = %v, want nil")
	case <-time.After(time.Second):
		t.Fatal("generated deltas did not keep the stream alive")
	}
}

func TestBuildSemanticIdleResetsOnGeneratedOutputItem(t *testing.T) {
	reader, writer := io.Pipe()
	body := wrapBuildSemanticIdle(reader, 300*time.Millisecond)
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		readDone <- err
	}()

	time.Sleep(180 * time.Millisecond)
	searchStarted := "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"ws_1\",\"type\":\"web_search_call\",\"status\":\"in_progress\",\"action\":{\"type\":\"search\",\"query\":\"current docs\"}}}\n\n"
	_, err := io.WriteString(writer, searchStarted)
	testutil.NoError(t, err)
	time.Sleep(180 * time.Millisecond)
	testutil.NoError(t, writer.Close())

	select {
	case err := <-readDone:
		testutil.NoError(t, err, "body read error = %v, want nil")
	case <-time.After(time.Second):
		t.Fatal("generated output item did not keep the stream alive")
	}
}

func TestBuildSSEActivityDetectorRecognizesSplitGeneratedEvents(t *testing.T) {
	for kind := range buildGeneratedDeltaEvents {
		t.Run(kind, func(t *testing.T) {
			var detector buildSSEActivityDetector
			event := fmt.Sprintf("event: %s\r\ndata: {\"type\":%q,\"delta\":\"x\"}\r\n\r\n", kind, kind)
			active := false
			for index := range event {
				active = detector.Observe([]byte(event[index:index+1])) || active
			}
			testutil.True(t, active, "split generated event was not recognized")
		})
	}
}

func TestBuildSSEActivityDetectorDoesNotRescanSplitLinePrefix(t *testing.T) {
	var detector buildSSEActivityDetector
	line := strings.Repeat("x", 256*1024)
	for index := range line {
		detector.Observe([]byte(line[index : index+1]))
		testutil.Equal(t, detector.scanOffset, len(detector.pending))
	}
	testutil.False(t, detector.Observe([]byte("\n\n")), "unknown long line must not count as activity")
	testutil.Falsef(t, len(detector.pending) != 0 || detector.scanOffset != 0, "detector retained consumed input: pending=%d offset=%d", len(detector.pending), detector.scanOffset)
}

func TestBuildSSEActivityDetectorRequiresJSONGeneratedEvent(t *testing.T) {
	var detector buildSSEActivityDetector
	testutil.False(t, detector.Observe([]byte("event: response.output_text.delta\ndata: not-json\n\n")), "malformed event unexpectedly counted as generated activity")
	testutil.False(t, detector.Observe([]byte(": keep-alive\n\n")), "keepalive unexpectedly counted as generated activity")
	testutil.False(t, detector.Observe([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"\"}\n\n")), "empty delta unexpectedly counted as generated activity")
	testutil.False(t, detector.Observe([]byte("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"web_search_call\"}}\n\n")), "unidentified output item unexpectedly counted as generated activity")
	testutil.False(t, detector.Observe([]byte("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"web_search_call\",\"status\":\"in_progress\"}}\n\n")), "anonymous output item unexpectedly counted as generated activity")
	testutil.False(t, detector.Observe([]byte("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"control_1\",\"type\":\"private_control\"}}\n\n")), "unknown output item unexpectedly counted as generated activity")
}

func TestBuildSSEActivityDetectorRecognizesGeneratedOutputItems(t *testing.T) {
	tests := []string{
		`{"type":"response.output_item.added","item":{"id":"ws_1","type":"web_search_call","status":"in_progress"}}`,
		`{"type":"response.output_item.added","item":{"id":"reasoning_1","type":"reasoning"}}`,
		`{"type":"response.output_item.done","item":{"call_id":"call_1","type":"function_call","name":"lookup","status":"completed"}}`,
	}
	for _, payload := range tests {
		var detector buildSSEActivityDetector
		testutil.Falsef(t, !detector.Observe([]byte("data: "+payload+"\n\n")), "generated output item was not recognized: %s", payload)
	}
}

func TestBuildSemanticIdlePausesWhileNotReading(t *testing.T) {
	reader, writer := io.Pipe()
	body := wrapBuildSemanticIdle(reader, 60*time.Millisecond)
	chunk := "event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"x\"}\n\n"
	writeErr := make(chan error, 2)
	go func() {
		_, err := io.WriteString(writer, chunk)
		writeErr <- err
	}()
	buf := make([]byte, len(chunk))
	n, err := body.Read(buf)
	testutil.Equal(t, err, nil)
	testutil.Equal(t, n, len(chunk))
	testutil.NoError(t, <-writeErr)
	time.Sleep(150 * time.Millisecond)
	testutil.False(t, body.(*semanticIdleReadCloser).TimedOut(), "idle timer fired while nobody was reading upstream")
	go func() {
		_, err := io.WriteString(writer, chunk)
		writeErr <- err
		_ = writer.Close()
	}()
	n, err = body.Read(buf)
	testutil.Equal(t, err, nil)
	testutil.Equal(t, n, len(chunk))
	testutil.NoError(t, <-writeErr)
	_, err = io.Copy(io.Discard, body)
	testutil.NoError(t, err)
}

func TestBuildSemanticIdleKeepalivesStillTimeoutWhileReading(t *testing.T) {
	reader, writer := io.Pipe()
	body := wrapBuildSemanticIdle(reader, 60*time.Millisecond)
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		readDone <- err
	}()
	go func() {
		defer writer.Close()
		for {
			if _, err := io.WriteString(writer, ": keep-alive\n\n"); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	select {
	case err := <-readDone:
		testutil.Falsef(t, !errors.Is(err, errGrokSemanticIdle), "body read error = %v, want ErrUpstreamStreamIdleTimeout", err)
	case <-time.After(time.Second):
		t.Fatal("keepalives while reading did not idle-timeout")
	}
}

func TestBuildSemanticIdleIgnoresStaleTimerCallbackAfterActivityReset(t *testing.T) {
	inner := &semanticIdleCountingReader{Reader: strings.NewReader("")}
	body := wrapBuildSemanticIdle(inner, time.Second).(*semanticIdleReadCloser)

	body.mu.Lock()
	body.readers = 1
	body.remaining = time.Second
	body.clockStart = time.Now()
	body.timer = time.AfterFunc(time.Hour, body.timeout)
	body.mu.Unlock()

	// Simulate an expired AfterFunc callback from the previous deadline arriving
	// after useful output has already refreshed clockStart and remaining.
	body.timeout()
	testutil.False(t, body.TimedOut(), "stale timer callback timed out the refreshed read deadline")

	body.mu.Lock()
	body.readers = 0
	body.stopClockLocked()
	body.mu.Unlock()
	testutil.NoError(t, body.Close())
	testutil.Equal(t, inner.closes.Load(), 1)
}

func TestBuildSemanticIdleStopsAfterEOFOrClose(t *testing.T) {
	t.Run("EOF", func(t *testing.T) {
		inner := &semanticIdleCountingReader{Reader: strings.NewReader("done")}
		body := wrapBuildSemanticIdle(inner, 20*time.Millisecond).(*semanticIdleReadCloser)
		_, err := io.ReadAll(body)
		testutil.NoError(t, err)
		time.Sleep(40 * time.Millisecond)
		testutil.False(t, body.TimedOut(), "timer fired after EOF")
		testutil.NoError(t, body.Close())
		testutil.Equal(t, inner.closes.Load(), 1)
	})

	t.Run("Close", func(t *testing.T) {
		inner := &semanticIdleCountingReader{Reader: strings.NewReader("")}
		body := wrapBuildSemanticIdle(inner, 20*time.Millisecond).(*semanticIdleReadCloser)
		testutil.NoError(t, body.Close())
		testutil.NoError(t, body.Close())
		time.Sleep(40 * time.Millisecond)
		testutil.False(t, body.TimedOut(), "timer fired after Close")
		testutil.Equal(t, inner.closes.Load(), 1)
	})
}

type semanticIdleCountingReader struct {
	io.Reader
	closes atomic.Int32
}

func (r *semanticIdleCountingReader) Close() error {
	r.closes.Add(1)
	return nil
}

func TestFirstGenerationIgnoresEmptyOutputItemIdentity(t *testing.T) {
	var d buildSSEActivityDetector
	d.Observe([]byte("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"id\":\"msg-1\"}}\n\n"))
	if d.generated {
		t.Fatal("control item ID must not release first-generation deadline")
	}
	d.Observe([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"))
	if !d.generated {
		t.Fatal("text must release first-generation deadline")
	}
}
