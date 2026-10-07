// Package hitl reads what a paused task is waiting for and builds the answer
// that resumes it. It knows the kagent HITL extension, not the terminal.
package hitl

import (
	"errors"
	"fmt"
	"strings"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
)

// Pending is the request a paused task holds. Only ReadPending makes one, so
// every answer is built against a request the task actually asked.
type Pending struct {
	TaskID  a2atype.TaskID
	Request Request
}

// Request is what the agent is waiting for: ToolApproval, AskUser or Unknown.
type Request interface{ isRequest() }

// ToolApproval asks a human to approve or reject tool calls.
type ToolApproval struct {
	// Tools are the calls to decide: a nested request's child tools, since its
	// top-level tool is the parent's continuation, not what is being authorized.
	Tools []kagenta2a.HITLTool
	// Hint is the request's explanation, or the status prose when it has none.
	Hint string
	// AskedBy is the subagent that asked, for a nested request.
	AskedBy string
	request *kagenta2a.ToolApprovalRequest
}

// AskUser asks a human one or more questions.
type AskUser struct {
	Questions []kagenta2a.HITLQuestion
	AskedBy   string
	request   *kagenta2a.AskUserRequest
}

// Unknown is a pause this build cannot answer: no extension payload, a type it
// does not know, or a payload it cannot use (Problem). It can only be discarded.
type Unknown struct {
	// Prose is the status message text, which is the request for prose-only pauses.
	Prose   string
	Problem error
}

func (ToolApproval) isRequest() {}
func (AskUser) isRequest()      {}
func (Unknown) isRequest()      {}

// ReadPending reads the request of an input-required task; any other task
// has none. A request this build cannot answer is Unknown, never dropped: a
// pause that renders nothing reads as a stalled agent.
func ReadPending(task *a2atype.Task) (Pending, bool) {
	if task == nil || task.Status.State != a2atype.TaskStateInputRequired {
		return Pending{}, false
	}
	return Pending{TaskID: task.ID, Request: readRequest(task.Status.Message)}, true
}

func readRequest(message *a2atype.Message) Request {
	prose := messageText(message)
	approval, err := kagenta2a.ParseToolApprovalRequest(message)
	if err != nil {
		return Unknown{Prose: prose, Problem: err}
	}
	if approval != nil {
		return toolApproval(approval, prose)
	}
	ask, err := kagenta2a.ParseAskUserRequest(message)
	if err != nil {
		return Unknown{Prose: prose, Problem: err}
	}
	if ask != nil {
		return askUser(ask, prose)
	}
	return Unknown{Prose: prose}
}

func toolApproval(request *kagenta2a.ToolApprovalRequest, prose string) Request {
	tools, askedBy := request.Tools, ""
	if request.Nested != nil {
		tools, askedBy = request.Nested.Tools, request.Nested.SubagentName
	}
	if err := decidable(tools); err != nil {
		return Unknown{Prose: prose, Problem: err}
	}
	hint := request.Hint
	if hint == "" {
		hint = prose
	}
	return ToolApproval{Tools: tools, Hint: hint, AskedBy: askedBy, request: request}
}

// decidable checks that every tool can be named exactly once in a response.
func decidable(tools []kagenta2a.HITLTool) error {
	if len(tools) == 0 {
		return errors.New("tool approval request has no tools to decide")
	}
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if tool.ID == "" {
			return fmt.Errorf("tool approval request has a %s call without an id", tool.Name)
		}
		if _, dup := seen[tool.ID]; dup {
			return fmt.Errorf("tool approval request repeats id %q", tool.ID)
		}
		seen[tool.ID] = struct{}{}
	}
	return nil
}

func askUser(request *kagenta2a.AskUserRequest, prose string) Request {
	if request.Nested != nil {
		// Which id a nested answer must echo is unsettled between the runtime and
		// the API validator, so guessing could misapply the answer.
		return Unknown{Prose: prose}
	}
	if len(request.Questions) == 0 {
		return Unknown{Prose: prose, Problem: errors.New("ask-user request has no questions")}
	}
	return AskUser{Questions: request.Questions, request: request}
}

// messageText is the message's text parts, which carry the request as prose.
func messageText(message *a2atype.Message) string {
	if message == nil {
		return ""
	}
	var text strings.Builder
	for _, part := range message.Parts {
		if part != nil {
			text.WriteString(part.Text())
		}
	}
	return strings.TrimSpace(text.String())
}
