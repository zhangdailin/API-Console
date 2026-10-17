package responses

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type discardStreamWriter struct{ header http.Header }

func (w *discardStreamWriter) Header() http.Header       { return w.header }
func (*discardStreamWriter) WriteHeader(int)             {}
func (*discardStreamWriter) Write(p []byte) (int, error) { return len(p), nil }
func (*discardStreamWriter) Flush()                      {}

func BenchmarkChatToResponsesStream(b *testing.B) {
	input := strings.Repeat("data: {\"id\":\"chat-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello world\"},\"finish_reason\":null}]}\n\n", 512) + "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	for i := 0; i < b.N; i++ {
		w := &discardStreamWriter{header: make(http.Header)}
		WriteStreamFromChatReader(w, CreateRequest{Model: "m"}, io.NopCloser(strings.NewReader(input)), StreamOptions{})
	}
}
