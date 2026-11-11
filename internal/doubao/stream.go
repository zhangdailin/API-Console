package doubao

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
)

func consumeStream(body io.Reader, emit func(string)) (string, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var event, data, userMessageID string
	var text, snapshot string
	// emitted tracks how much of the answer has reached the client. A snapshot
	// can arrive before the first delta (the opening words of an answer are
	// often delivered whole), and later deltas continue from there; emitting the
	// snapshot's unseen suffix keeps the two sources from dropping or repeating
	// text.
	emitted := 0
	emitUnseen := func(full string) {
		if len(full) <= emitted {
			return
		}
		emit(full[emitted:])
		emitted = len(full)
	}
	var images []creationImage
	seenImages := map[string]bool{}
	// shapes counts the event names and block layouts of the stream, never
	// their content, so an answer this parser could not read can be diagnosed.
	shapes := map[string]int{}
	flush := func() error {
		if event == "" {
			data = ""
			return nil
		}
		shapes[event]++
		var value map[string]any
		if data != "" && json.Unmarshal([]byte(data), &value) == nil {
			if event != "FULL_MSG_NOTIFY" && event != "CHUNK_DELTA" {
				blockShapes(value, event, shapes)
			}
			switch event {
			case "FULL_MSG_NOTIFY":
				if message, ok := value["message"].(map[string]any); ok {
					userMessageID, _ = message["message_id"].(string)
				}
			case "CHUNK_DELTA":
				if delta, _ := value["text"].(string); delta != "" {
					text += delta
					emit(delta)
					emitted = len(text)
				}
			case "STREAM_ERROR":
				return upstreamStreamError(value)
			case "STREAM_CHUNK", "STREAM_MSG_NOTIFY", "MSG_NOTIFY":
				messageID, _ := value["message_id"].(string)
				if messageID == "" {
					if message, ok := value["message"].(map[string]any); ok {
						messageID, _ = message["message_id"].(string)
					}
				}
				if messageID != "" && messageID == userMessageID {
					break
				}
				fragment, full := partitionText(value)
				if fragment != "" {
					// patch_type 1 appends a fragment; several can precede the
					// first CHUNK_DELTA, so both sources feed the same buffer.
					text += fragment
					emit(fragment)
					emitted = len(text)
				}
				if len(full) > len(snapshot) {
					snapshot = full
					if len(snapshot) > len(text) {
						emitUnseen(snapshot)
						text = snapshot
					}
				}
				// Image answers arrive as creation blocks with no text at all;
				// they have to be collected separately or the turn looks empty.
				for _, image := range creationImages(value) {
					if seenImages[image.url] {
						continue
					}
					seenImages[image.url] = true
					images = append(images, image)
				}
			}
		}
		event, data = "", ""
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return "", err
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(value)
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data += strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if err := flush(); err != nil {
		return "", err
	}
	if text == "" {
		text = snapshot
		emit(text)
	}
	if markdown := imageMarkdown(images); markdown != "" {
		text += markdown
		emit(markdown)
	}
	if text == "" {
		slog.Warn("doubao stream carried no readable answer", "shapes", shapeSummary(shapes))
	}
	return text, nil
}

// blockShapes records, for every content block in one frame, its block type,
// patch type and the names of its content fields.
func blockShapes(node any, event string, shapes map[string]int) {
	switch typed := node.(type) {
	case map[string]any:
		if blocks, ok := typed["content_block"].([]any); ok {
			for _, raw := range blocks {
				block, _ := raw.(map[string]any)
				content, _ := block["content"].(map[string]any)
				fields := make([]string, 0, len(content))
				for name, value := range content {
					// An empty placeholder field says nothing about the layout.
					if text, isText := value.(string); !isText || text != "" {
						fields = append(fields, name)
					}
				}
				sort.Strings(fields)
				shapes[fmt.Sprintf("%s/block_type=%v/patch_type=%v/%s", event, block["block_type"], block["patch_type"], strings.Join(fields, "+"))]++
			}
		}
		for _, child := range typed {
			blockShapes(child, event, shapes)
		}
	case []any:
		for _, child := range typed {
			blockShapes(child, event, shapes)
		}
	}
}

// shapeSummary renders the counted shapes in a stable order, bounded in size.
func shapeSummary(shapes map[string]int) string {
	names := make([]string, 0, len(shapes))
	for name := range shapes {
		names = append(names, name)
	}
	sort.Strings(names)
	var out strings.Builder
	for _, name := range names {
		if out.Len() > 1500 {
			out.WriteString(" ...")
			break
		}
		fmt.Fprintf(&out, "%s x%d; ", name, shapes[name])
	}
	return strings.TrimSpace(out.String())
}

type creationImage struct{ id, url string }

// creationImages extracts generated images from a creation block. The same
// block is repeated in later snapshots, so callers de-duplicate by URL.
func creationImages(value map[string]any) []creationImage {
	var out []creationImage
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			if block, ok := typed["creation_block"].(map[string]any); ok {
				if creations, ok := block["creations"].([]any); ok {
					for _, raw := range creations {
						entry, _ := raw.(map[string]any)
						image, _ := entry["image"].(map[string]any)
						if image == nil {
							continue
						}
						if url := creationImageURL(image); url != "" {
							id, _ := entry["id"].(string)
							out = append(out, creationImage{id: id, url: url})
						}
					}
				}
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return out
}

// creationImageURL prefers the unwatermarked original, then the watermarked
// original, then the preview and thumbnail variants.
func creationImageURL(image map[string]any) string {
	for _, key := range []string{"image_ori_raw", "image_ori", "image_preview", "image_thumb"} {
		variant, _ := image[key].(map[string]any)
		if variant == nil {
			continue
		}
		if url, _ := variant["url"].(string); strings.HasPrefix(url, "https://") {
			return url
		}
	}
	return ""
}

func imageMarkdown(images []creationImage) string {
	if len(images) == 0 {
		return ""
	}
	var out strings.Builder
	for index, image := range images {
		fmt.Fprintf(&out, "\n\n![generated image %d](%s)\n", index+1, image.url)
	}
	return out.String()
}

func snapshotText(value map[string]any) string {
	return collectText(value, false)
}

// partitionText splits one frame into appended fragments (patch_type 1) and the
// longest whole-answer snapshot (any other patch type). The web client uses
// both: an answer can open with fragments and continue with CHUNK_DELTA events.
func partitionText(value map[string]any) (fragment, snapshot string) {
	var fragments strings.Builder
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			if blocks, ok := typed["content_block"].([]any); ok {
				for _, raw := range blocks {
					block, _ := raw.(map[string]any)
					content, _ := block["content"].(map[string]any)
					textBlock, _ := content["text_block"].(map[string]any)
					text, _ := textBlock["text"].(string)
					if text == "" {
						continue
					}
					if patchType(block["patch_type"]) == 1 {
						fragments.WriteString(text)
					} else if len(text) > len(snapshot) {
						snapshot = text
					}
				}
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return fragments.String(), snapshot
}

func patchType(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case string:
		parsed, _ := strconv.Atoi(typed)
		return parsed
	}
	return 0
}

// deltaText returns text that a patch appends rather than replaces.
func deltaText(value map[string]any) string {
	return collectText(value, true)
}

// collectText walks a content tree for text blocks. patch_type 1 marks an
// appended fragment, so several blocks in one frame are concatenated; other
// patches carry the whole text so far and only the longest one counts.
func collectText(value map[string]any, appendOnly bool) string {
	var out strings.Builder
	var walk func(any) string
	walk = func(node any) string {
		switch typed := node.(type) {
		case map[string]any:
			if block, ok := typed["text_block"].(map[string]any); ok {
				if text, _ := block["text"].(string); text != "" {
					if appendOnly {
						out.WriteString(text)
					} else {
						return text
					}
				}
			}
			for _, child := range typed {
				if found := walk(child); found != "" {
					if !appendOnly {
						return found
					}
				}
			}
		case []any:
			for _, child := range typed {
				if found := walk(child); found != "" {
					if !appendOnly {
						return found
					}
				}
			}
		}
		return ""
	}
	found := walk(value)
	if appendOnly {
		return out.String()
	}
	return found
}

func upstreamStreamError(value map[string]any) error {
	code := errorCodeString(value["error_code"])
	if code == "710022004" {
		decision := sharkDecision(value)
		if decision.needsVerification() {
			// shark_admin's verify challenge can only be answered in a real
			// browser session. Replaying it here cannot succeed and only deepens
			// the risk window, so this error must stop retries and park the
			// account instead of entering the generic rate-limit path.
			return &verificationRequired{code: code, decision: decision}
		}
	}
	if code == "710022002" || code == "710022004" {
		return fmt.Errorf("status=429: doubao upstream rate limited (%s)", code)
	}
	return fmt.Errorf("doubao stream error: %s", code)
}

// errorCodeString renders a decoded JSON number as its integer decimal form.
// fmt.Sprint on the float64 produced by encoding/json prints 710022004 as
// 7.10022004e+08, which silently disabled the rate-limit classification.
func errorCodeString(value any) string {
	switch typed := value.(type) {
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	case json.Number:
		return typed.String()
	case string:
		return strings.TrimSpace(typed)
	}
	return strings.TrimSpace(fmt.Sprint(value))
}
