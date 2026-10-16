package responses

import (
	"bufio"
	"bytes"
	"io"

	"orchids-api/internal/util"
)

type SSEEvent struct {
	Event        string
	ID           string
	Retry        string
	Comments     []string
	Other        []string
	data         []string
	rawDataStart int
	rawDataEnd   int
	hasRawData   bool
	// raw is the frame exactly as it arrived, including its line endings and the
	// terminating blank line. writeTo prefers it so an untouched frame is
	// byte-identical to the upstream: the relay passes the native Responses
	// stream through instead of re-rendering it, so the client receives exactly
	// what the upstream wrote.
	raw []byte
}

// Data returns a read-only view of the raw frame for a single data line.
func (e SSEEvent) Data() []byte {
	if e.hasRawData {
		return e.raw[e.rawDataStart:e.rawDataEnd]
	}
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

func (e SSEEvent) HasData() bool { return e.hasRawData || len(e.data) > 0 }

func (e SSEEvent) writeTo(writer io.Writer) error {
	if len(e.raw) > 0 {
		n, err := writer.Write(e.raw)
		if err == nil && n != len(e.raw) {
			err = io.ErrShortWrite
		}
		return err
	}
	if len(e.data) == 1 && len(e.Comments) == 0 && len(e.Other) == 0 && e.ID == "" && e.Retry == "" {
		if framed, ok := writer.(interface{ WriteSSEString(string, string) error }); ok {
			return framed.WriteSSEString(e.Event, e.data[0])
		}
	}
	// A rewritten frame is still one write. In the pipe bridge each separate
	// write is a producer/consumer rendezvous and a transport deadline update.
	size := 1
	for _, comment := range e.Comments {
		size += len(comment) + 1
	}
	for _, field := range e.Other {
		size += len(field) + 1
	}
	for _, line := range e.data {
		size += len("data: ") + len(line) + 1
	}
	for _, field := range []struct{ prefix, value string }{{"event: ", e.Event}, {"id: ", e.ID}, {"retry: ", e.Retry}} {
		if field.value != "" {
			size += len(field.prefix) + len(field.value) + 1
		}
	}
	frame := make([]byte, 0, size)
	for _, comment := range e.Comments {
		frame = append(frame, comment...)
		frame = append(frame, '\n')
	}
	for _, field := range []struct{ prefix, value string }{{"event: ", e.Event}, {"id: ", e.ID}, {"retry: ", e.Retry}} {
		if field.value != "" {
			frame = append(frame, field.prefix...)
			frame = append(frame, field.value...)
			frame = append(frame, '\n')
		}
	}
	for _, field := range e.Other {
		frame = append(frame, field...)
		frame = append(frame, '\n')
	}
	for _, line := range e.data {
		frame = append(frame, "data: "...)
		frame = append(frame, line...)
		frame = append(frame, '\n')
	}
	frame = append(frame, '\n')
	n, err := writer.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
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
	scanner.Buffer(buffer[:], int(^uint(0)>>1))
	scanner.Split(splitLinesKeepingEnd)
	event := SSEEvent{}
	firstLine := true
	var rawFrame []byte
	flush := func() error {
		if !event.HasData() && len(event.Comments) == 0 && len(event.Other) == 0 && event.Event == "" && event.ID == "" && event.Retry == "" {
			rawFrame = rawFrame[:0]
			return nil
		}
		current := event
		current.raw = append([]byte(nil), rawFrame...)
		event = SSEEvent{}
		rawFrame = rawFrame[:0]
		return handle(current)
	}
	for scanner.Scan() {
		rawLine := scanner.Bytes()
		lineStart := len(rawFrame)
		rawFrame = append(rawFrame, rawLine...)
		line := bytes.TrimSuffix(bytes.TrimSuffix(rawLine, []byte("\n")), []byte("\r"))
		prefixLen := 0
		if firstLine {
			if bytes.HasPrefix(line, []byte("\xef\xbb\xbf")) {
				line = line[3:]
				prefixLen = 3
			}
			firstLine = false
		}
		if len(line) == 0 {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		field, value, found := bytes.Cut(line, []byte(":"))
		if found && len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		switch {
		case line[0] == ':':
			event.Comments = append(event.Comments, string(line))
		case !found:
			event.Other = append(event.Other, string(line))
		case bytes.Equal(field, []byte("event")):
			event.Event = string(value)
		case bytes.Equal(field, []byte("data")):
			if !event.hasRawData && len(event.data) == 0 {
				event.rawDataStart = lineStart + prefixLen + len(line) - len(value)
				event.rawDataEnd = event.rawDataStart + len(value)
				event.hasRawData = true
			} else {
				if event.hasRawData {
					event.data = append(event.data, string(rawFrame[event.rawDataStart:event.rawDataEnd]))
					event.hasRawData = false
				}
				event.data = append(event.data, string(value))
			}
		case bytes.Equal(field, []byte("id")):
			event.ID = string(value)
		case bytes.Equal(field, []byte("retry")):
			event.Retry = string(value)
		default:
			event.Other = append(event.Other, string(line))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}
