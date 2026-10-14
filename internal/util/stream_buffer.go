package util

import "sync"

// Only the small initial buffer is pooled. Scanner-owned growth for a large
// frame is discarded with the scanner rather than retained across requests.
var streamBufferPool = sync.Pool{New: func() any { return new([4096]byte) }}

func AcquireStreamBuffer() *[4096]byte { return streamBufferPool.Get().(*[4096]byte) }
func ReleaseStreamBuffer(buffer *[4096]byte) {
	if buffer != nil {
		clear(buffer[:])
		streamBufferPool.Put(buffer)
	}
}
