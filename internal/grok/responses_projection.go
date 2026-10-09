package grok

import (
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"orchids-api/internal/util"
)

func responsesObjectFromChat(model string, chat map[string]interface{}) map[string]interface{} {
	return responses.ObjectFromChatWithExtras(model, chat, grokChatOutputExtras)
}
func grokChatOutputExtras(message map[string]interface{}) []interface{} {
	out := []interface{}{}
	if thoughts := responses.InterfaceSlice(message["x_grok_reasoning"]); len(thoughts) > 0 {
		for _, raw := range thoughts {
			item, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			copy := responses.CloneStringInterfaceMap(item)
			copy["id"] = "rs_" + util.RandomHex(12)
			copy["status"] = "completed"
			out = append(out, copy)
		}
	}
	seen := map[string]bool{}
	for _, raw := range responses.InterfaceSlice(message["x_grok_searches"]) {
		search, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		key := searchIdentity(search)
		if seen[key] {
			continue
		}
		seen[key] = true
		item := responses.CloneStringInterfaceMap(search)
		if chatwire.ParseLooseStringAny(item["id"]) == "" {
			item["id"] = "ws_" + util.RandomHex(12)
		}
		out = append(out, item)
	}

	return out
}
