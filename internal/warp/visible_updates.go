package warp

import warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"

// messageActionPreview lets output classification and continuation commit use the
// same validated message changes. A stream owns its task exclusively. Message
// actions replace the slice and clone changed messages, so the original can be
// restored until the caller accepts the action.
type messageActionPreview struct {
	state    *continuationAccumulator
	action   *warpapi.ClientAction
	messages []*warpapi.Message
	created  *warpapi.Task
	checked  bool
	err      error
}

func (p *messageActionPreview) evaluate() error {
	if p.checked {
		return p.err
	}
	p.checked = true
	a := p.state
	if a.err != nil {
		p.err = a.err
		return p.err
	}
	switch p.action.WhichAction() {
	case warpapi.ClientAction_CreateTask_case:
		// A CreateTask can already contain paired server results. Validate it
		// without publishing the new root before those results are reviewed.
		candidate := continuationAccumulator{root: a.root}
		p.err = candidate.applyAction(p.action)
		if p.err == nil {
			p.created = candidate.root
		}
		return p.err
	case warpapi.ClientAction_AddMessagesToTask_case, warpapi.ClientAction_UpdateTaskMessage_case, warpapi.ClientAction_AppendToMessageContent_case:
	default:
		p.err = errContinuationState
		return p.err
	}
	if a.root != nil {
		before := a.root.GetMessages()
		defer a.root.SetMessages(before)
	}
	p.err = a.applyAction(p.action)
	if p.err == nil {
		p.messages = a.root.GetMessages()
	}
	return p.err
}

func (p *messageActionPreview) message(id string) (*warpapi.Message, error) {
	if err := p.evaluate(); err != nil {
		return nil, err
	}
	for _, m := range p.messages {
		if m.GetId() == id {
			return m, nil
		}
	}
	return nil, errContinuationState
}

func (p *messageActionPreview) commit() error {
	if !p.checked {
		return p.state.apply(p.action)
	}
	if p.err != nil {
		p.state.err = p.err
		return p.err
	}
	if p.created != nil {
		p.state.root = p.created
		return nil
	}
	p.state.root.SetMessages(p.messages)
	return nil
}

// visible projects the same masked text snapshot as task history.
// Once a real root exists, a delta must not be exposed as unmasked output.
func (p *messageActionPreview) visible(message *warpapi.Message) (*warpapi.Message, bool, error) {
	state, action := p.state, p.action
	update := action.WhichAction() == warpapi.ClientAction_UpdateTaskMessage_case || action.WhichAction() == warpapi.ClientAction_AppendToMessageContent_case
	if !update || state == nil || state.root == nil {
		return message, false, nil
	}
	if state.err != nil {
		return nil, false, state.err
	}
	// A mask is not permission to hide a native or unknown executable variant.
	if call := message.GetToolCall(); call != nil && call.WhichTool() != warpapi.Message_ToolCall_Server_case && call.WhichTool() != warpapi.Message_ToolCall_CallMcpTool_case {
		return message, false, nil
	}
	if result := message.GetToolCallResult(); result != nil && result.WhichResult() != warpapi.Message_ToolCallResult_Server_case && result.WhichResult() != warpapi.Message_ToolCallResult_CallMcpTool_case {
		return message, false, nil
	}
	effective, err := p.message(message.GetId())
	return effective, err == nil, err
}
