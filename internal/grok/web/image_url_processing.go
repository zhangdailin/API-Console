package web

import (
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

func sanitizeText(s string) string {
	if s == "" {
		return s
	}
	// Drop replacement chars introduced by invalid UTF-8 boundaries.
	s = strings.ReplaceAll(s, "\uFFFD", "")
	return s
}

func formatImageMarkdown(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	// Blank lines around images improve rendering in some clients.
	return "\n\n![](" + u + ")\n\n"
}

var reImageURLInText = regexp.MustCompile(`https?://[^\s"')>]+\.(?:png|jpe?g|webp|gif)(?:\?[^\s"')>]*)?`)
var reGrokAssetPathInText = regexp.MustCompile(`(?i)(?:^|[^\w/])((?:users|user)/[a-z0-9-]+/generated/[a-z0-9-]+(?:-part-0)?/image\.(?:png|jpe?g|webp|gif))`)

func extractImageURLsFromText(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	m := reImageURLInText.FindAllString(s, -1)
	if len(m) == 0 {
		return nil
	}
	return uniqueStrings(m)
}

func extractGrokAssetPathsFromText(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	ms := reGrokAssetPathInText.FindAllStringSubmatch(s, -1)
	if len(ms) == 0 {
		return nil
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if len(m) == 2 {
			out = append(out, m[1])
		}
	}
	return uniqueStrings(out)
}

// preferFullOverPart drops "-part-0" preview variants when the corresponding full URL is present.
// This is part of the stable contract:
// - Never emit -part-0 when full exists.
func preferFullOverPart(urls []string) []string {
	if len(urls) == 0 {
		return urls
	}
	set := map[string]struct{}{}
	for _, u := range urls {
		set[u] = struct{}{}
	}
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if strings.Contains(u, "-part-0/") {
			full := strings.ReplaceAll(u, "-part-0/", "/")
			if _, ok := set[full]; ok {
				continue
			}
		}
		out = append(out, u)
	}
	return out
}

func normalizeImageURLs(urls []string, n int) []string {
	return selectImageURLs(urls, false, n)
}

// normalizeGeneratedImageURLs is stricter than normalizeImageURLs:
// when Grok/local file URLs exist, they are preferred over arbitrary external links.
func normalizeGeneratedImageURLs(urls []string, n int) []string {
	return selectImageURLs(urls, true, n)
}

func selectImageURLs(urls []string, preferGrok bool, n int) []string {
	type scored struct {
		u     string
		score int
	}
	input := uniqueStrings(urls)
	all := make([]scored, 0, len(input))
	preferred := make([]scored, 0, len(input))
	for _, u := range input {
		sc := imageURLScore(u)
		if sc < 0 {
			continue
		}
		item := scored{u: strings.TrimSpace(u), score: sc}
		all = append(all, item)
		if sc >= 700 {
			preferred = append(preferred, item)
		}
	}
	candidates := all
	if preferGrok && len(preferred) > 0 {
		candidates = preferred
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].u < candidates[j].u
		}
		return candidates[i].score > candidates[j].score
	})
	out := make([]string, 0, len(candidates))
	for _, it := range candidates {
		out = append(out, it.u)
	}
	out = preferFullOverPart(out)
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

func imageURLScore(raw string) int {
	u := strings.TrimSpace(raw)
	if !isLikelyImageURL(u) {
		return -1
	}
	lu := strings.ToLower(u)
	if strings.HasPrefix(lu, "/grok/v1/files/image/") || strings.HasPrefix(lu, "/v1/files/image/") {
		return 1000
	}

	score := 100
	if strings.HasPrefix(lu, "http://127.0.0.1:") || strings.HasPrefix(lu, "http://localhost:") {
		if strings.Contains(lu, "/grok/v1/files/image/") || strings.Contains(lu, "/v1/files/image/") {
			score = 980
		}
	}
	if parsed, err := url.Parse(u); err == nil {
		host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
		switch {
		case host == "assets.grok.com":
			score = 900
		case host == "grok.com" || strings.HasSuffix(host, ".grok.com"):
			score = 700
		case strings.Contains(host, "encrypted-tbn"):
			score = 10
		}
		path := strings.ToLower(parsed.EscapedPath())
		if strings.Contains(path, "/generated/") {
			score += 30
		}
		if strings.Contains(path, "/image.") {
			score += 10
		}
		if strings.Contains(path, "-part-0/") {
			score -= 120
		}
		query := strings.ToLower(parsed.RawQuery)
		if strings.Contains(query, "thumbnail") || strings.Contains(query, "thumb") {
			score -= 30
		}
	}
	return score
}

func isLikelyImageURL(u string) bool {
	u = strings.TrimSpace(u)
	if u == "" {
		return false
	}
	if strings.HasPrefix(u, "/grok/v1/files/image/") || strings.HasPrefix(u, "/v1/files/image/") {
		return true
	}
	lu := strings.ToLower(u)
	if strings.HasPrefix(lu, "http://") || strings.HasPrefix(lu, "https://") {
		parsed, err := url.Parse(u)
		if err == nil {
			base := strings.TrimSpace(path.Base(parsed.EscapedPath()))
			if strings.HasPrefix(base, ".") {
				return false
			}
		}
		// Quick allow if it clearly ends with an image extension (ignore query).
		cut := lu
		if q := strings.IndexByte(cut, '?'); q >= 0 {
			cut = cut[:q]
		}
		if strings.HasSuffix(cut, ".jpg") || strings.HasSuffix(cut, ".jpeg") || strings.HasSuffix(cut, ".png") || strings.HasSuffix(cut, ".webp") || strings.HasSuffix(cut, ".gif") {
			return true
		}
		// assets.grok.com generated image paths. Some Grok streams only expose an
		// asset id, which is served through /content instead of a file extension.
		if strings.Contains(lu, "assets.grok.com/") && (strings.Contains(lu, "/generated/") || strings.Contains(lu, "/image") || strings.HasSuffix(lu, "/content")) {
			return true
		}
		return false
	}
	return false
}

func isLikelyImageAssetPath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	if strings.HasPrefix(path.Base(p), ".") {
		return false
	}
	// Reject JSON blobs or echoed prompts.
	if strings.HasPrefix(p, "{") || strings.Contains(p, "Image Generation:") {
		return false
	}
	// Reject anything with whitespace (asset paths/urls shouldn't contain spaces/newlines).
	if strings.ContainsAny(p, " \t\r\n") {
		return false
	}
	lp := strings.ToLower(p)
	if strings.HasSuffix(lp, ".jpg") || strings.HasSuffix(lp, ".jpeg") || strings.HasSuffix(lp, ".png") || strings.HasSuffix(lp, ".webp") || strings.HasSuffix(lp, ".gif") {
		return true
	}
	return false
}
