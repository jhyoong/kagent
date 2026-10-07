package hitl

import (
	"errors"
	"fmt"
	"strings"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/transcript"
)

// Verdict is the decision on one tool.
type Verdict int

const (
	Undecided Verdict = iota
	Approved
	Rejected
)

// ApprovalForm collects one decision per requested tool. It is built from a
// Pending, so an answer always names the task and tools that were asked.
type ApprovalForm struct {
	taskID   a2atype.TaskID
	req      ToolApproval
	verdicts []Verdict
	reasons  []string
}

// NewApprovalForm starts an undecided form for a pending tool approval.
func NewApprovalForm(pending Pending) (*ApprovalForm, bool) {
	req, ok := pending.Request.(ToolApproval)
	if !ok {
		return nil, false
	}
	return &ApprovalForm{
		taskID:   pending.TaskID,
		req:      req,
		verdicts: make([]Verdict, len(req.Tools)),
		reasons:  make([]string, len(req.Tools)),
	}, true
}

// Request is what the form decides.
func (f *ApprovalForm) Request() ToolApproval { return f.req }

// Tools are the calls to decide, in request order.
func (f *ApprovalForm) Tools() []kagenta2a.HITLTool { return f.req.Tools }

// Verdict is tool i's decision so far.
func (f *ApprovalForm) Verdict(i int) Verdict {
	if !f.valid(i) {
		return Undecided
	}
	return f.verdicts[i]
}

// Reason is tool i's rejection reason; approved and undecided tools have none.
func (f *ApprovalForm) Reason(i int) string {
	if !f.valid(i) {
		return ""
	}
	return f.reasons[i]
}

// Approve approves tool i.
func (f *ApprovalForm) Approve(i int) {
	if f.valid(i) {
		f.verdicts[i], f.reasons[i] = Approved, ""
	}
}

// Reject rejects tool i; an empty reason is allowed.
func (f *ApprovalForm) Reject(i int, reason string) {
	if f.valid(i) {
		f.verdicts[i], f.reasons[i] = Rejected, strings.TrimSpace(reason)
	}
}

// ApproveAll approves every tool, replacing earlier rejections.
func (f *ApprovalForm) ApproveAll() {
	for i := range f.verdicts {
		f.Approve(i)
	}
}

// Decided counts the tools with a verdict.
func (f *ApprovalForm) Decided() int {
	decided := 0
	for _, verdict := range f.verdicts {
		if verdict != Undecided {
			decided++
		}
	}
	return decided
}

// Complete reports whether every tool is decided, which the runtime requires.
func (f *ApprovalForm) Complete() bool { return f.Decided() == len(f.verdicts) }

// Answer is the message that resumes the task, and the record the transcript
// shows for it. The message carries fallback prose for the agent and the
// structured decisions under the HITL extension, which the runtime acts on.
func (f *ApprovalForm) Answer(contextID string) (*a2atype.Message, transcript.ApprovalRecord, error) {
	if !f.Complete() {
		return nil, transcript.ApprovalRecord{}, errors.New("every tool needs a decision before the answer is sent")
	}
	approvals := make([]kagenta2a.ToolApproval, len(f.req.Tools))
	for i, tool := range f.req.Tools {
		approvals[i] = kagenta2a.ToolApproval{ID: tool.ID, Approved: f.verdicts[i] == Approved, RejectionReason: f.reasons[i]}
	}
	response := &kagenta2a.ToolApprovalResponse{Type: kagenta2a.HITLTypeToolApprovalResponse, Approvals: approvals}
	if err := kagenta2a.ValidateToolApprovalResponse(f.req.request, response); err != nil {
		return nil, transcript.ApprovalRecord{}, fmt.Errorf("failed to build tool approval for task %s: %w", f.taskID, err)
	}

	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart(approvalText(f.req.Tools, approvals)))
	message.TaskID, message.ContextID = f.taskID, contextID
	if err := kagenta2a.AttachHITL(message, response); err != nil {
		return nil, transcript.ApprovalRecord{}, fmt.Errorf("failed to attach tool approval for task %s: %w", f.taskID, err)
	}
	return message, transcript.ApprovalRecord{Tools: f.req.Tools, Decisions: approvals, AskedBy: f.req.AskedBy}, nil
}

func (f *ApprovalForm) valid(i int) bool { return i >= 0 && i < len(f.verdicts) }

// approvalText is the prose beside the decisions, as the web UI writes it.
func approvalText(tools []kagenta2a.HITLTool, approvals []kagenta2a.ToolApproval) string {
	lines := make([]string, 0, len(approvals))
	for i, approval := range approvals {
		name := tools[i].Name
		if name == "" {
			name = "tool"
		}
		if approval.Approved {
			lines = append(lines, "Approved: "+name)
			continue
		}
		line := "Rejected: " + name
		if approval.RejectionReason != "" {
			line += "\nReason: " + approval.RejectionReason
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
