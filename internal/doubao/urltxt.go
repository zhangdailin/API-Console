package doubao

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"orchids-api/internal/httpclient"
)

const (
	// urlTextMaxBytes bounds what the gateway reads from a caller-supplied URL.
	// A page larger than this is truncated rather than refused: the answer is
	// still useful and the alternative is a silent failure.
	urlTextMaxBytes = 512 << 10
	// urlTextMaxBytesBinary rejects content that is clearly not text before the
	// whole payload is pulled into the prompt.
	urlTextMaxBytesBinary = 4 << 10
)

// fetchURLText downloads a caller-supplied URL and returns it as prompt text.
//
// Doubao reads a URL written in the prompt through its own browsing path, but
// that path is the model's choice: it may answer from the link title alone. The
// gateway fetches the bytes itself so a file or page URL reliably reaches the
// model as content.
//
// The request is an SSRF sink, so it goes through the shared outbound guard:
// public addresses only, no credentials in the URL, bounded redirects, and the
// dial pinned to the validated address.
func (c *Client) fetchURLText(ctx context.Context, rawURL string) (string, error) {
	proxy := httpclient.ProxyFuncFromConfig(c.cfg)
	target, err := httpclient.CheckRemoteFetchTarget(ctx, rawURL, proxy != nil)
	if err != nil {
		return "", err
	}
	client := httpclient.NewRemoteFetchClient(30_000_000_000, proxy)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.userAgent())
	req.Header.Set("Accept", "text/*,application/json,application/xml;q=0.9,*/*;q=0.5")
	response, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return "", fmt.Errorf("url fetch failed: status=%d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, urlTextMaxBytes))
	if err != nil {
		return "", err
	}
	if !isTextPayload(body, response.Header.Get("Content-Type")) {
		return "", fmt.Errorf("url content is not text (%s)", first(response.Header.Get("Content-Type"), "unknown type"))
	}
	return normalizeFetchedText(body), nil
}

// isTextPayload decides from the declared type and, when the type is absent or
// generic, from the bytes themselves.
func isTextPayload(body []byte, contentType string) bool {
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		switch {
		case strings.HasPrefix(mediaType, "text/"):
			return true
		case mediaType == "application/json", mediaType == "application/xml", mediaType == "application/xhtml+xml":
			return true
		case strings.HasPrefix(mediaType, "image/"), strings.HasPrefix(mediaType, "audio/"), strings.HasPrefix(mediaType, "video/"):
			return false
		}
	}
	sample := body
	if len(sample) > urlTextMaxBytesBinary {
		sample = sample[:urlTextMaxBytesBinary]
	}
	return utf8.Valid(sample) && !strings.ContainsRune(string(sample), 0)
}

// normalizeFetchedText trims a byte-order mark and normalizes line endings so the
// prompt does not carry transport artefacts.
func normalizeFetchedText(body []byte) string {
	text := strings.TrimPrefix(string(body), "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.TrimSpace(text)
}

// expandPromptURLs appends the text behind each link in the prompt.
//
// Doubao reads a URL written in the prompt through its own browsing path, and
// that path is the model's choice: it can answer from the link title. Fetching
// the content here makes a file or page URL reliable, and the original URL stays
// in the prompt so the model can still cite it.
//
// A URL that cannot be fetched is skipped rather than failing the turn: a bad
// link should degrade to the model's own browsing, not to a hard error.
func (c *Client) expandPromptURLs(ctx context.Context, promptText string) (string, error) {
	urls := promptURLs(promptText, maxPromptURLs)
	if len(urls) == 0 {
		return promptText, nil
	}
	var out strings.Builder
	out.WriteString(promptText)
	for _, rawURL := range urls {
		text, err := c.fetchURLText(ctx, rawURL)
		if err != nil {
			continue
		}
		if text == "" {
			continue
		}
		fmt.Fprintf(&out, "\n\n[content of %s]\n%s\n", rawURL, text)
	}
	return out.String(), nil
}

// maxPromptURLs bounds how many links one turn expands. A prompt listing dozens
// of links would otherwise turn into dozens of outbound requests.
const maxPromptURLs = 3

// promptURLs extracts distinct http(s) links in the order they appear.
func promptURLs(text string, limit int) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '"', '\'', '<', '>', '(', ')', '[', ']', '，', '。', '；', '、':
			return true
		}
		return false
	})
	seen := make(map[string]struct{}, limit)
	out := make([]string, 0, limit)
	for _, field := range fields {
		candidate := strings.TrimRight(field, ".,;:!?)]}\u3002\uff0c")
		if !isHTTPURL(candidate) {
			continue
		}
		if _, found := seen[candidate]; found {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
		if len(out) >= limit {
			break
		}
	}
	return out
}
