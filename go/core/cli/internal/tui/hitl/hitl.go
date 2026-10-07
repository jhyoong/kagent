// Package hitl reads the human-in-the-loop requests a paused kagent task holds
// and builds the decisions that resume it.
//
// It speaks kagent's adk_request_confirmation protocol as documented in
// docs/architecture/human-in-the-loop.md ("HITL Protocol Reference"), the same
// wire format the web UI sends. It is semantic: no terminal I/O and no Bubble Tea.
package hitl

import (
	"errors"
	"fmt"
	"slices"

	"trpc.group/trpc-go/trpc-a2a-go/protocol"
)

const (
	// confirmationName is the function call ADK emits when a tool waits for a human.
	confirmationName = "adk_request_confirmation"
	// askUserName is kagent's built-in question tool; its confirmation carries questions.
	askUserName = "ask_user"
)

// Tool is one call waiting for approval. ID is the key its decision is sent under:
// the original call's ID, or the inner call's ID when a subagent asked.
type Tool struct {
	ID   string
	Name string
	Args map[string]any
}

// Question is one ask_user question. Without choices the answer is free text.
type Question struct {
	Question string
	Choices  []string
	Multiple bool
}

// Request is what a paused task waits for.
type Request interface{ isRequest() }

// ToolApproval asks for a decision on each of Tools.
type ToolApproval struct {
	Tools []Tool
	// Hint is the runtime's description of what needs approval; empty when tools disagree.
	Hint string
	// Subagent names the subagent that asked, when one did.
	Subagent string
}

// AskUser asks Questions; the answers are positional.
type AskUser struct {
	Questions []Question
	// Subagent names the subagent that asked, when one did.
	Subagent string
}

// Unknown is a request this client cannot answer; it can only be declined.
type Unknown struct{ Problem error }

func (ToolApproval) isRequest() {}
func (AskUser) isRequest()      {}
func (Unknown) isRequest()      {}

// Pending is the request a paused task holds. A decision must name TaskID, or the
// runtime starts a new task instead of resuming this one.
type Pending struct {
	TaskID  string
	Request Request
}

// ReadPending reads the request of an input-required task.
func ReadPending(task *protocol.Task) (Pending, bool) {
	if task == nil || task.Status.State != protocol.TaskStateInputRequired || task.Status.Message == nil {
		return Pending{}, false
	}
	request, ok := ReadRequest(task.Status.Message.Parts)
	if !ok {
		return Pending{}, false
	}
	return Pending{TaskID: task.ID, Request: request}, true
}

// LastPending is the request of the newest input-required task. A session pauses on
// one task at a time; tasks are oldest first, as the sessions API lists them.
func LastPending(tasks []*protocol.Task) (Pending, bool) {
	for _, task := range slices.Backward(tasks) {
		if pending, ok := ReadPending(task); ok {
			return pending, true
		}
	}
	return Pending{}, false
}

// ReadRequest reads the confirmations among parts. ok is false when there are none.
// Confirmations this client cannot answer read as Unknown, so they can be declined.
//
// The rules mirror the web UI: an ask_user confirmation, direct or relayed by a
// subagent as its only call, is a question; anything else asks for approval of
// the original calls, or of the subagent's inner calls when it relays them.
func ReadRequest(parts []protocol.Part) (Request, bool) {
	var confirmations []confirmation
	for _, part := range parts {
		if c, ok := readConfirmation(part); ok {
			confirmations = append(confirmations, c)
		}
	}
	if len(confirmations) == 0 {
		return nil, false
	}

	var (
		tools     []Tool
		questions *AskUser
		hints     = map[string]struct{}{}
		subagents = map[string]struct{}{}
	)
	for _, c := range confirmations {
		if c.err != nil {
			return Unknown{Problem: c.err}, true
		}
		if ask, ok := c.askUser(); ok {
			if questions != nil {
				return Unknown{Problem: errors.New("more than one ask_user request")}, true
			}
			questions = &ask
			continue
		}
		tools = append(tools, c.tools()...)
		hints[c.hint] = struct{}{}
		subagents[c.subagent] = struct{}{}
	}

	if questions != nil {
		if len(tools) > 0 {
			// The runtime applies answers to every pending confirmation, which would approve the tools.
			return Unknown{Problem: errors.New("questions and tool approvals are pending together")}, true
		}
		return *questions, true
	}
	for _, tool := range tools {
		if tool.ID == "" {
			return Unknown{Problem: fmt.Errorf("tool %q has no call ID to decide on", tool.Name)}, true
		}
	}
	approval := ToolApproval{Tools: tools}
	if len(hints) == 1 {
		for hint := range hints {
			approval.Hint = hint
		}
	}
	if len(subagents) == 1 {
		for subagent := range subagents {
			approval.Subagent = subagent
		}
	}
	return approval, true
}

// IsConfirmation reports whether part is an adk_request_confirmation call.
func IsConfirmation(part protocol.Part) bool {
	data, ok := confirmationData(part)
	return ok && data != nil
}

// confirmation is one adk_request_confirmation call: the original call it holds,
// and the subagent's inner calls when the original call is a subagent.
type confirmation struct {
	original call
	inner    []call
	hint     string
	subagent string
	// err is set when the confirmation is malformed.
	err error
}

type call struct {
	ID   string
	Name string
	Args map[string]any
}

func (c confirmation) askUser() (AskUser, bool) {
	switch {
	case c.original.Name == askUserName && len(c.inner) == 0:
		return AskUser{Questions: readQuestions(c.original.Args)}, true
	case len(c.inner) == 1 && c.inner[0].Name == askUserName:
		return AskUser{Questions: readQuestions(c.inner[0].Args), Subagent: c.subagent}, true
	}
	return AskUser{}, false
}

func (c confirmation) tools() []Tool {
	calls := c.inner
	if len(calls) == 0 {
		calls = []call{c.original}
	}
	tools := make([]Tool, len(calls))
	for i, call := range calls {
		tools[i] = Tool(call)
	}
	return tools
}

func readConfirmation(part protocol.Part) (confirmation, bool) {
	data, ok := confirmationData(part)
	if !ok {
		return confirmation{}, false
	}
	args, _ := data["args"].(map[string]any)
	original, ok := readCall(args["originalFunctionCall"])
	if !ok {
		return confirmation{err: errors.New("approval request without the call it holds")}, true
	}
	c := confirmation{original: original}
	toolConfirmation, _ := args["toolConfirmation"].(map[string]any)
	c.hint, _ = toolConfirmation["hint"].(string)
	payload, _ := toolConfirmation["payload"].(map[string]any)
	c.subagent, _ = payload["subagent_name"].(string)
	if raw, present := payload["hitl_parts"]; present && raw != nil {
		parts, ok := raw.([]any)
		if !ok {
			return confirmation{err: errors.New("subagent approval request with malformed inner calls")}, true
		}
		for _, raw := range parts {
			fields, _ := raw.(map[string]any)
			inner, ok := readCall(fields["originalFunctionCall"])
			if !ok {
				return confirmation{err: errors.New("subagent approval request with malformed inner calls")}, true
			}
			c.inner = append(c.inner, inner)
		}
	}
	return c, true
}

// confirmationData is the data of an adk_request_confirmation call part.
func confirmationData(part protocol.Part) (map[string]any, bool) {
	dataPart := asDataPart(part)
	if dataPart == nil {
		return nil, false
	}
	data, ok := dataPart.Data.(map[string]any)
	if !ok || data["name"] != confirmationName {
		return nil, false
	}
	if MetadataValue(dataPart.Metadata, "type") != "function_call" || MetadataValue(dataPart.Metadata, "is_long_running") != true {
		return nil, false
	}
	return data, true
}

func readCall(raw any) (call, bool) {
	fields, ok := raw.(map[string]any)
	if !ok {
		return call{}, false
	}
	name, _ := fields["name"].(string)
	if name == "" {
		return call{}, false
	}
	id, _ := fields["id"].(string)
	args, _ := fields["args"].(map[string]any)
	return call{ID: id, Name: name, Args: args}, true
}

func readQuestions(args map[string]any) []Question {
	raw, _ := args["questions"].([]any)
	questions := make([]Question, 0, len(raw))
	for _, item := range raw {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		question := Question{}
		question.Question, _ = fields["question"].(string)
		question.Multiple, _ = fields["multiple"].(bool)
		choices, _ := fields["choices"].([]any)
		for _, choice := range choices {
			if text, ok := choice.(string); ok {
				question.Choices = append(question.Choices, text)
			}
		}
		questions = append(questions, question)
	}
	return questions
}

// MetadataValue reads a kagent part metadata key, which runtimes prefix with
// "kagent_" or "adk_".
func MetadataValue(metadata map[string]any, key string) any {
	if value, ok := metadata["kagent_"+key]; ok {
		return value
	}
	return metadata["adk_"+key]
}

// asDataPart reads a data part; the A2A client decodes parts as pointers, and parts
// built in memory are values.
func asDataPart(part protocol.Part) *protocol.DataPart {
	switch p := part.(type) {
	case *protocol.DataPart:
		return p
	case protocol.DataPart:
		return &p
	}
	return nil
}
