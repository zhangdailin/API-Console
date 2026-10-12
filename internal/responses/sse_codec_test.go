package responses

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"orchids-api/internal/httpserver"
)

func TestConsumeSSEDataRetainedAndRewritten(t *testing.T) {
	input := "\uFEFFevent: first\r\ndata: one\r\n\r\nevent: second\ndata: two\ndata: three\n\nevent: empty\ndata:\n\n"
	var events []SSEEvent
	if err := ConsumeSSE(strings.NewReader(input), func(event SSEEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	for i, want := range []string{"one", "two\nthree", ""} {
		if !events[i].HasData() || string(events[i].Data()) != want {
			t.Fatalf("event %d data=%q, want %q", i, events[i].Data(), want)
		}
	}
	var raw bytes.Buffer
	for _, event := range events {
		if err := event.WriteFrame(&raw); err != nil {
			t.Fatal(err)
		}
	}
	if raw.String() != input {
		t.Fatalf("raw frames changed: %q", raw.String())
	}
	events[0].SetData("replacement")
	if string(events[0].Data()) != "replacement" {
		t.Fatalf("rewritten data=%q", events[0].Data())
	}
}

func TestRewrittenSSEFrameUsesCheckedWriter(t *testing.T) {
	event := SSEEvent{Event: "response.output_text.delta"}
	event.SetData(`{"delta":"hello"}`)
	var out bytes.Buffer
	writer := &httpserver.CheckedStreamWriter{Target: &out}
	if err := event.WriteFrame(writer); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "event: response.output_text.delta\ndata: {\"delta\":\"hello\"}\n\n"; got != want {
		t.Fatalf("frame=%q, want %q", got, want)
	}

	// Fields outside event/data must still be emitted by the generic renderer.
	event.ID = "cursor"
	out.Reset()
	if err := event.WriteFrame(writer); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "id: cursor\n") {
		t.Fatalf("rewritten frame lost id: %q", out.String())
	}
	event.ID = ""
	short := &frameShortWriter{}
	writer = &httpserver.CheckedStreamWriter{Target: short}
	if err := event.WriteFrame(writer); !errors.Is(err, io.ErrShortWrite) || !errors.Is(writer.Err, io.ErrShortWrite) {
		t.Fatalf("short write error=%v, stored=%v", err, writer.Err)
	}
}
