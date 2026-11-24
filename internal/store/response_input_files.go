package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// responseInputFileRef locates a value moved out of InputItems. It is a sidecar
// on the trusted response row, never a marker interpreted from client input.
type responseInputFileRef struct {
	Item   int    `json:"item"`
	Part   int    `json:"part"`
	Group  string `json:"group,omitempty"`
	Field  string `json:"field"`
	Digest string `json:"digest"`
}

func decodeResponseInput(raw json.RawMessage) ([]any, error) {
	var items []any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	err := decoder.Decode(&items)
	return items, err
}

func filePart(value any) map[string]any {
	block, _ := value.(map[string]any)
	kind, _ := block["type"].(string)
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "input_file", "file":
		return block
	}
	return nil
}

func inlineFileData(value string) bool {
	raw := strings.TrimSpace(value)
	header, _, ok := strings.Cut(raw, ",")
	const prefix, suffix = "data:", ";base64"
	return ok && len(header) > len(prefix)+len(suffix) &&
		strings.EqualFold(header[:len(prefix)], prefix) && strings.EqualFold(header[len(header)-len(suffix):], suffix)
}

func detachResponseInputFiles(row *StoredResponse) (map[string]string, error) {
	if !row.ExternalizeInputFiles ||
		(!bytes.Contains(row.InputItems, []byte(`"file_data"`)) && !bytes.Contains(row.InputItems, []byte(`"data"`)) &&
			!bytes.Contains(row.InputItems, []byte(`"file"`)) && !bytes.Contains(row.InputItems, []byte(`"source"`))) {
		return nil, nil
	}
	items, err := decodeResponseInput(row.InputItems)
	if err != nil {
		return nil, err
	}
	files := make(map[string]string)
	for i, raw := range items {
		item, _ := raw.(map[string]any)
		parts, _ := item["content"].([]any)
		for j, candidate := range parts {
			part := filePart(candidate)
			if part == nil {
				continue
			}
			for _, group := range []string{"", "file", "source"} {
				fields := part
				if group != "" {
					fields, _ = part[group].(map[string]any)
				}
				for _, field := range []string{"file_data", "data", "file", "source"} {
					value, _ := fields[field].(string)
					if !inlineFileData(value) {
						continue
					}
					hash := sha256.Sum256([]byte(value))
					digest := hex.EncodeToString(hash[:])
					files[digest] = value
					row.InputFileRefs = append(row.InputFileRefs, responseInputFileRef{
						Item: i, Part: j, Group: group, Field: field, Digest: digest,
					})
					delete(fields, field)
				}
			}
		}
	}
	if len(files) == 0 {
		return nil, nil
	}
	row.InputItems, err = json.Marshal(items)
	return files, err
}

var errInvalidResponseInputFile = errors.New("invalid stored response file reference")

func restoreResponseInputFiles(row *StoredResponse, load func(string) (string, error)) error {
	if len(row.InputFileRefs) == 0 {
		return nil
	}
	items, err := decodeResponseInput(row.InputItems)
	if err != nil {
		return errInvalidResponseInputFile
	}
	files := make(map[string]string)
	for _, ref := range row.InputFileRefs {
		if ref.Item < 0 || ref.Item >= len(items) {
			return errInvalidResponseInputFile
		}
		item, _ := items[ref.Item].(map[string]any)
		parts, _ := item["content"].([]any)
		if ref.Part < 0 || ref.Part >= len(parts) {
			return errInvalidResponseInputFile
		}
		fields := filePart(parts[ref.Part])
		if ref.Group != "" {
			fields, _ = fields[ref.Group].(map[string]any)
		}
		if fields == nil || fields[ref.Field] != nil {
			return errInvalidResponseInputFile
		}
		value, found := files[ref.Digest]
		if !found {
			value, err = load(ref.Digest)
			if err != nil {
				return err
			}
			files[ref.Digest] = value
		}
		fields[ref.Field] = value
	}
	row.InputItems, err = json.Marshal(items)
	row.InputFileRefs = nil
	row.ExternalizeInputFiles = true
	return err
}
