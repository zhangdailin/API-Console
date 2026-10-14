package grok

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/url"
	"strings"

	"encoding/json"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/responses"
	"orchids-api/internal/util"
)

// checkedStreamWriter lives in internal/httpserver: it is a generic io.Writer +
// http.Flusher failure recorder, not Anthropic-search behaviour.
type checkedStreamWriter = httpserver.CheckedStreamWriter

func nullableProtocolString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type messageSearchState struct {
	index int
	done  bool
}

func anthropicSearchTool(tool anthropicTool) (map[string]any, error) {
	if len(tool.BlockedDomains) > 0 && len(tool.ExcludedDomains) > 0 {
		return nil, fmt.Errorf("blocked_domains and excluded_domains are aliases; supply only one")
	}
	excluded := tool.ExcludedDomains
	if len(tool.BlockedDomains) > 0 {
		excluded = tool.BlockedDomains
	}
	if len(tool.AllowedDomains) > 0 && len(excluded) > 0 {
		return nil, fmt.Errorf("allowed_domains and excluded domains are mutually exclusive")
	}
	filters := map[string]any{}
	for key, domains := range map[string][]string{"allowed_domains": tool.AllowedDomains, "excluded_domains": excluded} {
		if len(domains) > 5 {
			return nil, fmt.Errorf("%s cannot exceed 5 domains", key)
		}
		for _, domain := range domains {
			if strings.TrimSpace(domain) == "" {
				return nil, fmt.Errorf("%s must contain non-empty domains", key)
			}
		}
		if len(domains) > 0 {
			filters[key] = append([]string(nil), domains...)
		}
	}
	result := map[string]any{"type": "web_search"}
	if len(filters) > 0 {
		result["filters"] = filters
	}
	return result, nil
}

func searchIdentity(item map[string]any) string {
	id := chatwire.ParseLooseStringAny(item["id"])
	if id == "" {
		raw, _ := json.Marshal(item["action"])
		id = fmt.Sprintf("%x", sha256.Sum256(raw))[:24]
	}
	if !strings.HasPrefix(id, "srvtoolu_") {
		id = "srvtoolu_" + id
	}
	return id
}

func searchContent(item map[string]any) []any {
	id := searchIdentity(item)
	action, _ := item["action"].(map[string]any)
	query := chatwire.ParseLooseStringAny(action["query"])
	use := map[string]any{"type": "server_tool_use", "id": id, "name": "web_search", "input": map[string]any{"query": query}}
	var content any = []any{}
	if status := chatwire.ParseLooseStringAny(item["status"]); status == "failed" || status == "incomplete" || action == nil {
		content = map[string]any{"type": "web_search_tool_result_error", "error_code": "unavailable"}
	} else {
		hits := []any{}
		seen := map[string]bool{}
		for _, raw := range responses.InterfaceSlice(action["sources"]) {
			source, _ := raw.(map[string]any)
			link := chatwire.ParseLooseStringAny(source["url"])
			parsed, err := url.Parse(link)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || seen[link] {
				continue
			}
			seen[link] = true
			hits = append(hits, map[string]any{"type": "web_search_result", "url": link, "title": util.FirstNonEmpty(chatwire.ParseLooseStringAny(source["title"]), parsed.Host)})
			if len(hits) >= 50 {
				break
			}
		}
		content = hits
	}
	return []any{use, map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": content}}
}

func (s *anthropicStreamState) writeSearch(w io.Writer, item map[string]any, done bool) {
	id := searchIdentity(item)
	if s.searches == nil {
		s.searches = map[string]*messageSearchState{}
	}
	state := s.searches[id]
	if state != nil && state.done {
		return
	}
	action, _ := item["action"].(map[string]any)
	query := chatwire.ParseLooseStringAny(action["query"])
	if state == nil {
		s.closeTextualBlocks(w)
		state = &messageSearchState{index: s.startBlock(w, map[string]any{"type": "server_tool_use", "id": id, "name": "web_search", "input": map[string]any{}})}
		s.searches[id] = state
	}
	// The final query snapshot is authoritative; emitting partial snapshots as
	// JSON deltas would concatenate different objects when the query changes.
	if !done {
		return
	}
	input, _ := json.Marshal(map[string]any{"query": query})
	writeAnthropicSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": state.index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
	s.closeBlock(w, state.index)
	blocks := searchContent(item)
	index := s.startBlock(w, blocks[1].(map[string]any))
	s.closeBlock(w, index)
	state.done = true
}

func chatCitations(raw []any) []any {
	var result []any
	for _, value := range raw {
		annotation, _ := value.(map[string]any)
		citation, _ := annotation["url_citation"].(map[string]any)
		if citation == nil {
			citation = annotation
		}
		link := chatwire.ParseLooseStringAny(citation["url"])
		parsed, err := url.Parse(link)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			continue
		}
		result = append(result, map[string]any{"type": "web_search_result_location", "url": link, "title": chatwire.ParseLooseStringAny(citation["title"]), "cited_text": chatwire.ParseLooseStringAny(citation["cited_text"])})
	}
	return result
}

func (s *anthropicStreamState) writeCitations(w io.Writer, raw []any) {
	if s.citations == nil {
		s.citations = map[string]bool{}
	}
	for _, value := range chatCitations(raw) {
		citation := value.(map[string]any)
		encoded, _ := json.Marshal(citation)
		key := string(encoded)
		if s.citations[key] {
			continue
		}
		s.citations[key] = true
		if s.textIndex < 0 {
			s.closeTextualBlocks(w)
			s.textIndex = s.startBlock(w, map[string]any{"type": "text", "text": ""})
		}
		writeAnthropicSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": s.textIndex, "delta": map[string]any{"type": "citations_delta", "citation": citation}})
	}
}
