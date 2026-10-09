package responses

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"

	"orchids-api/internal/util"
)

const maxEventBytes = 8 << 20

type SSEEvent struct {
	Event    string
	ID       string
	Retry    string
	Comments []string
	Other    []string
	data     []string
	// raw is the frame exactly as it arrived, including its line endings and the
	// terminating blank line. writeTo prefers it so an untouched frame is
	// byte-identical to the upstream: the relay passes the native Responses
	// stream through instead of re-rendering it, so the client receives exactly
	// what the upstream wrote.
	raw []byte
}

func (e SSEEvent) Data() []byte {
	if len(e.data) == 0 {
		return nil
	}
	size := len(e.data) - 1
	for _, line := range e.data {
		size += len(line)
	}
	joined := make([]byte, size)
	offset := 0
	for index, line := range e.data {
		if index > 0 {
			joined[offset] = '\n'
			offset++
		}
		offset += copy(joined[offset:], line)
	}
	return joined
}

func (e SSEEvent) HasData() bool { return len(e.data) > 0 }

func (e SSEEvent) writeTo(writer io.Writer) error {
	if len(e.raw) > 0 {
		_, err := writer.Write(e.raw)
		return err
	}
	for _, comment := range e.Comments {
		if _, err := fmt.Fprintln(writer, comment); err != nil {
			return err
		}
	}
	if e.Event != "" {
		if _, err := fmt.Fprintf(writer, "event: %s\n", e.Event); err != nil {
			return err
		}
	}
	if e.ID != "" {
		if _, err := fmt.Fprintf(writer, "id: %s\n", e.ID); err != nil {
			return err
		}
	}
	if e.Retry != "" {
		if _, err := fmt.Fprintf(writer, "retry: %s\n", e.Retry); err != nil {
			return err
		}
	}
	for _, field := range e.Other {
		if _, err := fmt.Fprintln(writer, field); err != nil {
			return err
		}
	}
	for _, line := range e.data {
		if _, err := fmt.Fprintf(writer, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(writer)
	return err
}

// splitLinesKeepingEnd is bufio.ScanLines without the line-ending rewrite: the
// token keeps its own "\n" or "\r\n" so a relayed frame reproduces the upstream
// bytes exactly. The blank line that ends a frame is a token of its own.
func splitLinesKeepingEnd(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i+1], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func ConsumeSSE(source io.Reader, handle func(SSEEvent) error) error {
	scanner := bufio.NewScanner(source)
	buffer := util.AcquireStreamBuffer()
	defer util.ReleaseStreamBuffer(buffer)
	scanner.Buffer(buffer[:], maxEventBytes)
	scanner.Split(splitLinesKeepingEnd)
	event := SSEEvent{}
	eventBytes := 0
	firstLine := true
	var rawFrame []byte
	flush := func() error {
		if len(event.data) == 0 && len(event.Comments) == 0 && len(event.Other) == 0 && event.Event == "" && event.ID == "" && event.Retry == "" {
			rawFrame = rawFrame[:0]
			return nil
		}
		current := event
		current.raw = append([]byte(nil), rawFrame...)
		event = SSEEvent{}
		eventBytes = 0
		rawFrame = rawFrame[:0]
		return handle(current)
	}
	for scanner.Scan() {
		rawLine := scanner.Text()
		rawFrame = append(rawFrame, rawLine...)
		line := strings.TrimSuffix(strings.TrimSuffix(rawLine, "\n"), "\r")
		if firstLine {
			line = strings.TrimPrefix(line, "\uFEFF")
			firstLine = false
		}
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		eventBytes += len(line)
		if eventBytes > maxEventBytes {
			return fmt.Errorf("grok Build Responses SSE 单事件超过 %d MiB", maxEventBytes>>20)
		}
		field, value, found := strings.Cut(line, ":")
		if found && strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch {
		case strings.HasPrefix(line, ":"):
			event.Comments = append(event.Comments, line)
		case !found:
			event.Other = append(event.Other, line)
		case field == "event":
			event.Event = value
		case field == "data":
			event.data = append(event.data, value)
		case field == "id":
			event.ID = value
		case field == "retry":
			event.Retry = value
		default:
			event.Other = append(event.Other, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}
