package web

import (
	"net/http"
	"strings"
	"unicode/utf8"
)

type streamMarkupFilter struct {
	pending  string
	inTool   bool
	inRender bool
}

const (
	streamToolStartTag   = "xai:tool_usage_card"
	streamToolEndTag     = "</xai:tool_usage_card>"
	streamRenderStartTag = "<grok:render"
	streamRenderEndTag   = "</grok:render>"
)

func asciiLowerByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if asciiLowerByte(a[i]) != asciiLowerByte(b[i]) {
			return false
		}
	}
	return true
}

func indexFoldASCII(s, sub string) int {
	if sub == "" {
		return 0
	}
	n := len(sub)
	if len(s) < n {
		return -1
	}
	first := asciiLowerByte(sub[0])
	for i := 0; i <= len(s)-n; i++ {
		if asciiLowerByte(s[i]) != first {
			continue
		}
		if equalFoldASCII(s[i:i+n], sub) {
			return i
		}
	}
	return -1
}

// Streaming sanitizer/tokenizer.
// Goal: never leak tool/render markup, never corrupt UTF-8, and keep streaming responsive.
func (f *streamMarkupFilter) feed(chunk string) string {
	if f == nil || chunk == "" {
		return ""
	}
	f.pending += chunk
	// Bound memory
	if len(f.pending) > 64*1024 {
		f.pending = f.pending[len(f.pending)-64*1024:]
	}

	var out strings.Builder
	endsWithNewline := false
	writeOut := func(s string) {
		if s == "" {
			return
		}
		out.WriteString(s)
		endsWithNewline = s[len(s)-1] == '\n'
	}

	for {
		if f.inTool {
			end := indexFoldASCII(f.pending, streamToolEndTag)
			if end < 0 {
				// wait for more data
				break
			}
			raw := f.pending[:end+len(streamToolEndTag)]
			if line := extractToolUsageCardText(raw); line != "" {
				if out.Len() > 0 && !endsWithNewline {
					writeOut("\n")
				}
				writeOut(line)
				writeOut("\n")
			}
			f.pending = f.pending[end+len(streamToolEndTag):]
			f.inTool = false
			continue
		}
		if f.inRender {
			end := indexFoldASCII(f.pending, streamRenderEndTag)
			if end < 0 {
				break
			}
			f.pending = f.pending[end+len(streamRenderEndTag):]
			f.inRender = false
			continue
		}

		idxTool := indexFoldASCII(f.pending, streamToolStartTag)
		idxRender := indexFoldASCII(f.pending, streamRenderStartTag)
		idx := -1
		kind := ""
		if idxTool >= 0 {
			idx = idxTool
			kind = "tool"
		}
		if idxRender >= 0 && (idx < 0 || idxRender < idx) {
			idx = idxRender
			kind = "render"
		}

		if idx < 0 {
			// No markers. Emit everything except a tail to avoid cutting potential markers.
			keep := 512
			if len(f.pending) <= keep {
				break
			}
			safe := validUTF8Prefix(f.pending[:len(f.pending)-keep])
			safe = sanitizeText(safe)
			if safe != "" {
				writeOut(safe)
			}
			f.pending = f.pending[len(f.pending)-keep:]
			break
		}

		// Emit prefix before marker
		prefix := validUTF8Prefix(f.pending[:idx])
		prefix = sanitizeText(prefix)
		if prefix != "" {
			writeOut(prefix)
		}
		f.pending = f.pending[idx:]
		if kind == "tool" {
			f.inTool = true
		} else {
			f.inRender = true
		}
	}

	return out.String()
}

func (f *streamMarkupFilter) flush() string {
	if f == nil {
		return ""
	}
	if f.inTool || f.inRender {
		return ""
	}
	if strings.TrimSpace(f.pending) == "" {
		return ""
	}
	out := sanitizeText(stripToolAndRenderMarkup(validUTF8Prefix(f.pending)))
	f.pending = ""
	return out
}

func validUTF8Prefix(s string) string {
	if s == "" || utf8.ValidString(s) {
		return s
	}
	// Trim bytes until valid UTF-8.
	b := []byte(s)
	for len(b) > 0 {
		b = b[:len(b)-1]
		if utf8.Valid(b) {
			return string(b)
		}
	}
	return ""
}

func sanitizeUpstreamText(raw string) string {
	return sanitizeText(stripToolAndRenderMarkup(raw))
}

const streamImageRefTailKeep = 1024

type streamTextImageRefCollector struct {
	tail      string
	urls      []string
	assets    []string
	seenURLs  map[string]struct{}
	seenAsset map[string]struct{}
}

func newStreamTextImageRefCollector() *streamTextImageRefCollector {
	return &streamTextImageRefCollector{
		urls:      make([]string, 0, 8),
		assets:    make([]string, 0, 8),
		seenURLs:  map[string]struct{}{},
		seenAsset: map[string]struct{}{},
	}
}

func streamRefMaybePresent(s string) bool {
	if s == "" {
		return false
	}
	return strings.Contains(s, "http://") ||
		strings.Contains(s, "https://") ||
		strings.Contains(s, "assets.grok.com") ||
		strings.Contains(s, "/generated/") ||
		strings.Contains(s, "users/") ||
		strings.Contains(s, "user/") ||
		strings.Contains(s, ".png") ||
		strings.Contains(s, ".jpg") ||
		strings.Contains(s, ".jpeg") ||
		strings.Contains(s, ".webp") ||
		strings.Contains(s, ".gif")
}

func keepLastBytes(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	return s[len(s)-maxLen:]
}

func (c *streamTextImageRefCollector) feed(raw string) {
	if c == nil || strings.TrimSpace(raw) == "" {
		return
	}
	scan := raw
	if c.tail != "" {
		scan = c.tail + raw
	}
	if streamRefMaybePresent(scan) {
		for _, u := range extractImageURLsFromText(scan) {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			if _, ok := c.seenURLs[u]; ok {
				continue
			}
			c.seenURLs[u] = struct{}{}
			c.urls = append(c.urls, u)
		}
		for _, p := range extractGrokAssetPathsFromText(scan) {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, ok := c.seenAsset[p]; ok {
				continue
			}
			c.seenAsset[p] = struct{}{}
			c.assets = append(c.assets, p)
		}
	}
	c.tail = keepLastBytes(scan, streamImageRefTailKeep)
}

func (c *streamTextImageRefCollector) emit(emitURL func(string)) {
	if c == nil || emitURL == nil {
		return
	}
	for _, u := range c.urls {
		emitURL(u)
	}
	for _, p := range c.assets {
		emitURL("https://assets.grok.com/" + strings.TrimPrefix(p, "/"))
	}
}

func forEachImageCandidateFromValue(value any, includeStructured bool, includeAssetLike bool, assetLimit int, emitURL func(string)) {
	if emitURL == nil {
		return
	}
	if includeStructured {
		for _, u := range extractImageURLs(value) {
			emitURL(u)
		}
	}
	for _, u := range extractRenderableImageLinks(value) {
		emitURL(u)
	}
	if !includeAssetLike {
		return
	}
	for _, p := range collectAssetLikeStrings(value, assetLimit) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if isLikelyImageURL(p) {
			emitURL(p)
			continue
		}
		if isLikelyImageAssetPath(p) {
			emitURL("https://assets.grok.com/" + strings.TrimPrefix(p, "/"))
		}
	}
}

func debugHeaderMap(headers http.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for k, values := range headers {
		if isSensitiveUpstreamHeader(k) {
			out[k] = "[redacted]"
			continue
		}
		out[k] = strings.Join(values, ", ")
	}
	return out
}
