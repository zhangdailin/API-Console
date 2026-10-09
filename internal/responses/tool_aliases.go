package responses

import "io"

// SetData replaces the frame's data lines and drops its raw copy.
//
// A changed payload discards the original bytes; writeTo renders the parsed fields.
func (e *SSEEvent) SetData(lines ...string) {
	e.data = lines
	e.raw = nil
}

// WriteTo renders the frame. An untouched frame is written back byte-for-byte
// from its raw copy, so a relayed stream reproduces the upstream exactly.
func (e SSEEvent) WriteFrame(writer io.Writer) error {
	return e.writeTo(writer)
}
