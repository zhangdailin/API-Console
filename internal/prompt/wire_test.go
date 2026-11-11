package prompt

import (
	"encoding/json"
	"testing"
)

// Chat Completions nests the media payload; this struct is flat. Without the
// decoder the URL was dropped and a provider received an empty image block.
func TestContentBlockFlattensOpenAINestedMedia(t *testing.T) {
	var msg Message
	raw := `{"role":"user","content":[
		{"type":"text","text":"describe"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}},
		{"type":"file","file":{"file_data":"data:text/plain;base64,aGk=","filename":"a.txt"}}
	]}`
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	blocks := msg.Content.GetBlocks()
	if len(blocks) != 3 {
		t.Fatalf("blocks=%d", len(blocks))
	}
	if blocks[1].URL != "data:image/png;base64,QUJD" {
		t.Fatalf("image url lost: %+v", blocks[1])
	}
	if blocks[2].FileData != "data:text/plain;base64,aGk=" || blocks[2].FileName != "a.txt" {
		t.Fatalf("file payload lost: %+v", blocks[2])
	}
}

// Some clients send image_url as a bare string instead of the object form.
func TestContentBlockAcceptsBareImageURLString(t *testing.T) {
	var msg Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"image_url","image_url":"https://example.com/a.png"}]}`), &msg); err != nil {
		t.Fatal(err)
	}
	if got := msg.Content.GetBlocks()[0].URL; got != "https://example.com/a.png" {
		t.Fatalf("url=%q", got)
	}
}
