// Package transcript models what a TUI conversation shows: structured entries
// projected from A2A tasks and stream events, and how each one renders.
//
// It is semantic: no terminal I/O and no Bubble Tea. Chat owns which entries
// are folded and when a turn starts; this package owns what entries mean and
// look like.
package transcript

import "github.com/kagent-dev/kagent/go/core/cli/internal/tui/hitl"

// Entry is one block of the transcript.
type Entry interface{ isEntry() }

// UserMessage is prose the reader sent.
type UserMessage struct{ Text string }

// AgentText is prose the agent produced.
type AgentText struct{ Text string }

// ToolActivity is one tool call and, once it arrives, its result, paired by call ID.
type ToolActivity struct {
	ID      string
	Name    string
	Args    any
	Outcome ToolOutcome
}

// ApprovalRecord is a completed tool approval: what was asked and what was decided.
// Approved and Reasons are positional with Tools.
type ApprovalRecord struct {
	Tools    []hitl.Tool
	Approved []bool
	Reasons  []string
	// Subagent names the subagent that asked, when one did.
	Subagent string
}

// AnswerRecord is a completed ask-user exchange. Answers are positional: the
// nth answer answers the nth question. Declined means the user gave up the
// questions instead of answering them.
type AnswerRecord struct {
	Questions []hitl.Question
	Answers   [][]string
	Declined  bool
	// Subagent names the subagent that asked, when one did.
	Subagent string
}

// BannerKind says how loudly a Banner reads.
type BannerKind int

const (
	// BannerInfo is state the reader should notice but need not act on.
	BannerInfo BannerKind = iota
	// BannerError is a failure: of the task, the transport, or the protocol.
	BannerError
)

// Banner is a line of client-authored status between conversation entries.
type Banner struct {
	Kind BannerKind
	Text string
}

func (UserMessage) isEntry()    {}
func (AgentText) isEntry()      {}
func (ToolActivity) isEntry()   {}
func (ApprovalRecord) isEntry() {}
func (AnswerRecord) isEntry()   {}
func (Banner) isEntry()         {}

// ToolOutcome is where a tool call stands.
type ToolOutcome interface{ isToolOutcome() }

// Running is a call whose result has not arrived.
type Running struct{}

// Returned is a call the tool executed. Failed is the tool's own error report.
type Returned struct {
	Response any
	Failed   bool
}

// AwaitingApproval is a call the runtime held for a human decision.
type AwaitingApproval struct{}

// NotRun is a call the runtime refused because a human rejected it.
type NotRun struct{}

func (Running) isToolOutcome()          {}
func (Returned) isToolOutcome()         {}
func (AwaitingApproval) isToolOutcome() {}
func (NotRun) isToolOutcome()           {}

// RecordFor is the entry for a decision sent on a request: an approval record,
// or an answer record for questions. ok is false for a request this client
// cannot read, which leaves nothing to record but the fact of a decision.
func RecordFor(request hitl.Request, decision hitl.Decision) (Entry, bool) {
	switch request := request.(type) {
	case hitl.ToolApproval:
		record := ApprovalRecord{
			Tools:    request.Tools,
			Approved: make([]bool, len(request.Tools)),
			Reasons:  make([]string, len(request.Tools)),
			Subagent: request.Subagent,
		}
		for i, tool := range request.Tools {
			record.Approved[i] = decision.Approved(tool.ID)
			if !record.Approved[i] {
				record.Reasons[i] = decision.Reason(tool.ID)
			}
		}
		return record, true
	case hitl.AskUser:
		return AnswerRecord{
			Questions: request.Questions,
			Answers:   decision.Answers,
			Declined:  decision.Type != hitl.Approve || len(decision.Answers) == 0,
			Subagent:  request.Subagent,
		}, true
	}
	return nil, false
}

// Folds records which entries are expanded: all flips the default, and an
// index in flipped deviates from it.
type Folds struct {
	all     bool
	flipped map[int]struct{}
}

// Expanded reports whether entry i renders expanded.
func (f Folds) Expanded(i int) bool {
	_, flipped := f.flipped[i]
	return f.all != flipped
}

// Toggle flips entry i against the current default.
func (f *Folds) Toggle(i int) {
	if _, flipped := f.flipped[i]; flipped {
		delete(f.flipped, i)
		return
	}
	if f.flipped == nil {
		f.flipped = map[int]struct{}{}
	}
	f.flipped[i] = struct{}{}
}

// ToggleAll flips the default and forgets individual flips, so every entry follows it.
func (f *Folds) ToggleAll() {
	f.all = !f.all
	f.flipped = nil
}
