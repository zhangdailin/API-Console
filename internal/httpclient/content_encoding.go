package httpclient

import (
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// DecodeContentEncoding replaces a compressed response body with a reader of
// its plaintext and drops the encoding headers. Only the first listed coding
// is decoded; an unknown coding leaves the response untouched. Closing the new
// body closes the decoder and the original body.
func DecodeContentEncoding(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if idx := strings.Index(encoding, ","); idx >= 0 {
		encoding = strings.TrimSpace(encoding[:idx])
	}
	if encoding == "" || encoding == "identity" {
		return nil
	}

	original := resp.Body
	var (
		reader  io.Reader
		closers []io.Closer
	)
	switch encoding {
	case "gzip":
		gz, err := gzip.NewReader(original)
		if err != nil {
			return err
		}
		reader = gz
		closers = append(closers, gz)
	case "deflate":
		fr := flate.NewReader(original)
		reader = fr
		closers = append(closers, fr)
	case "br":
		reader = brotli.NewReader(original)
	case "zstd":
		zr, err := zstd.NewReader(original)
		if err != nil {
			return err
		}
		reader = zr
		closers = append(closers, closeFunc(func() error { zr.Close(); return nil }))
	default:
		return nil
	}
	closers = append(closers, original)
	resp.Body = decodedBody{Reader: reader, closers: closers}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return nil
}

type decodedBody struct {
	io.Reader
	closers []io.Closer
}

type closeFunc func() error

func (fn closeFunc) Close() error { return fn() }

func (r decodedBody) Close() error {
	var firstErr error
	for _, closer := range r.closers {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
