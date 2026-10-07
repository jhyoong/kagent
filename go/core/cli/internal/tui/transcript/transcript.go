// Package transcript models what a TUI conversation shows: structured entries
// projected from A2A tasks and stream events, and how each one renders.
//
// It is semantic: no terminal I/O and no Bubble Tea. Chat owns which entries
// exist and which are folded; this package owns what they mean and look like.
package transcript

import kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"

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

// ApprovalRecord is a completed tool approval exchange, kept as one event.
// Tools is what was asked (the child's tools for a nested request); it is empty
// when the request was not in view, which renders the decisions generically.
type ApprovalRecord struct {
	Tools     []kagenta2a.HITLTool
	Decisions []kagenta2a.ToolApproval
	// AskedBy names the subagent that asked, when one did.
	AskedBy string
}

// AnswerRecord is a completed ask-user exchange. Answers are positional: the
// nth answer answers the nth question.
type AnswerRecord struct {
	Questions []kagenta2a.HITLQuestion
	Answers   [][]string
	// AskedBy names the subagent that asked, when one did.
	AskedBy string
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
