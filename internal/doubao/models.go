package doubao

import "strings"

// Model is a stable public alias and its observed web-client route metadata.
type Model struct {
	ID               string
	Name             string
	ModeID           string
	ModelItemKey     string
	AgentMode        int
	ConversationMode string
	ReasoningEffort  int
}

var models = []Model{
	{ID: "doubao", Name: "Doubao", ModeID: "1", ModelItemKey: "0", AgentMode: 2, ConversationMode: "1", ReasoningEffort: 4},
	{ID: "doubao-expert", Name: "Doubao Expert", ModeID: "1", ModelItemKey: "3", AgentMode: 2, ConversationMode: "1", ReasoningEffort: 4},
	{ID: "doubao-work", Name: "Doubao Work", ModeID: "3", ModelItemKey: "9", AgentMode: 1, ConversationMode: "2", ReasoningEffort: 4},
	{ID: "doubao-pro", Name: "Doubao Pro", ModeID: "3", ModelItemKey: "5", AgentMode: 1, ConversationMode: "2", ReasoningEffort: 4},
}

func Models() []Model { return append([]Model(nil), models...) }

func FindModel(id string) (Model, bool) {
	for _, model := range models {
		if strings.EqualFold(strings.TrimSpace(model.ID), strings.TrimSpace(id)) {
			return model, true
		}
	}
	return Model{}, false
}
