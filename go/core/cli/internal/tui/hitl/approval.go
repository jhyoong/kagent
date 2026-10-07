package hitl

import (
	"errors"
	"strings"

	"trpc.group/trpc-go/trpc-a2a-go/protocol"
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
	pending  Pending
	req      ToolApproval
	verdicts []Verdict
	reasons  []string
}

// NewApprovalForm starts an undecided form for a pending tool approval.
func NewApprovalForm(pending Pending) (*ApprovalForm, bool) {
	req, ok := pending.Request.(ToolApproval)
	if !ok || len(req.Tools) == 0 {
		return nil, false
	}
	return &ApprovalForm{
		pending:  pending,
		req:      req,
		verdicts: make([]Verdict, len(req.Tools)),
		reasons:  make([]string, len(req.Tools)),
	}, true
}

// Request is what the form decides.
func (f *ApprovalForm) Request() ToolApproval { return f.req }

// Tools are the calls to decide, in request order.
func (f *ApprovalForm) Tools() []Tool { return f.req.Tools }

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

// Complete reports whether every tool is decided; a partial decision would let
// the runtime approve the rest by default.
func (f *ApprovalForm) Complete() bool { return f.Decided() == len(f.verdicts) }

// Answer is the message that resumes the task, and the decision it carries.
func (f *ApprovalForm) Answer(contextID string) (protocol.Message, Decision, error) {
	if !f.Complete() {
		return protocol.Message{}, Decision{}, errors.New("every tool needs a decision before the answer is sent")
	}
	approved := make([]bool, len(f.verdicts))
	for i, verdict := range f.verdicts {
		approved[i] = verdict == Approved
	}
	decision := approvalDecision(f.req.Tools, approved, f.reasons)
	return f.pending.Message(contextID, decision, approvalLabel(decision, approved)), decision, nil
}

func (f *ApprovalForm) valid(i int) bool { return i >= 0 && i < len(f.verdicts) }
