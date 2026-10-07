package tui

import (
	"errors"
	"fmt"
	"strings"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/hitl"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/theme"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/transcript"
)

// prompt is what a paused turn shows in place of the composer. The chat owns
// ctrl+x (discard) and the hint line while a discard is being confirmed.
type prompt interface {
	// key handles one key and reports whether the answer is ready to send.
	key(msg tea.KeyMsg) (send bool)
	// answer is the message that resumes the task and the transcript's record of it.
	answer(contextID string) (*a2atype.Message, transcript.Entry, error)
	// view is the prompt body at width columns, without the hint line.
	view(width int) string
	hints() string
}

// newPrompt picks the prompt for a pending request; agent names the asker when no subagent did.
func newPrompt(agent string, pending hitl.Pending) prompt {
	switch req := pending.Request.(type) {
	case hitl.ToolApproval:
		if form, ok := hitl.NewApprovalForm(pending); ok {
			return newApprovalPrompt(agent, form)
		}
	case hitl.AskUser:
		questions := make([]string, 0, len(req.Questions))
		for _, question := range req.Questions {
			questions = append(questions, question.Question)
		}
		return discardPrompt{
			heading: "⏸ The agent is asking a question this view cannot answer yet.",
			prose:   strings.Join(questions, " "),
		}
	case hitl.Unknown:
		return discardPrompt{
			heading: "⏸ The agent is waiting for something this kagent version cannot answer.",
			prose:   req.Prose,
			problem: req.Problem,
		}
	}
	return discardPrompt{heading: "⏸ The agent is waiting for something this kagent version cannot answer."}
}

// discardPrompt shows a request that can only be discarded.
type discardPrompt struct {
	heading string
	prose   string
	problem error
}

func (discardPrompt) key(tea.KeyMsg) bool { return false }

func (discardPrompt) answer(string) (*a2atype.Message, transcript.Entry, error) {
	return nil, nil, errors.New("this request cannot be answered here")
}

func (p discardPrompt) view(width int) string {
	lines := []string{theme.PromptStyle().Render(p.heading)}
	if p.prose != "" {
		lines = append(lines, `  "`+p.prose+`"`)
	}
	if p.problem != nil {
		lines = append(lines, theme.ErrorStyle().Render("  "+p.problem.Error()))
	}
	return wrapLines(lines, width)
}

func (discardPrompt) hints() string { return "ctrl+x discard the request" }

// approvalPrompt decides each requested tool; enter sends once every tool is decided.
type approvalPrompt struct {
	asker  string
	form   *hitl.ApprovalForm
	cursor int
	// showArgs holds the tools whose full arguments are shown.
	showArgs map[int]bool
	// reason edits the cursor tool's rejection reason; nil when not editing.
	reason *textinput.Model
}

func newApprovalPrompt(agent string, form *hitl.ApprovalForm) *approvalPrompt {
	asker := form.Request().AskedBy
	if asker == "" {
		asker = agent
	}
	return &approvalPrompt{asker: asker, form: form, showArgs: map[int]bool{}}
}

func (p *approvalPrompt) key(msg tea.KeyMsg) bool {
	if p.reason != nil {
		switch msg.String() {
		case "enter":
			p.form.Reject(p.cursor, p.reason.Value())
			p.reason = nil
			p.next()
		case "esc":
			p.reason = nil // back, with the earlier decision kept
		default:
			updated, _ := p.reason.Update(msg)
			p.reason = &updated
		}
		return false
	}
	last := len(p.form.Tools()) - 1
	switch msg.String() {
	case "a":
		p.form.Approve(p.cursor)
		p.next()
	case "r":
		input := textinput.New()
		input.Prompt = "reason › "
		input.Placeholder = "optional"
		input.SetValue(p.form.Reason(p.cursor))
		input.Focus()
		p.reason = &input
	case "A":
		p.form.ApproveAll()
	case "up":
		p.cursor = max(p.cursor-1, 0)
	case "down":
		p.cursor = min(p.cursor+1, last)
	case " ", "space":
		p.showArgs[p.cursor] = !p.showArgs[p.cursor]
	case "enter":
		return p.form.Complete()
	}
	return false
}

// next moves to the following undecided tool, so deciding in order needs no arrows.
func (p *approvalPrompt) next() {
	tools := len(p.form.Tools())
	for step := 1; step < tools; step++ {
		if i := (p.cursor + step) % tools; p.form.Verdict(i) == hitl.Undecided {
			p.cursor = i
			return
		}
	}
}

func (p *approvalPrompt) answer(contextID string) (*a2atype.Message, transcript.Entry, error) {
	message, record, err := p.form.Answer(contextID)
	if err != nil {
		return nil, nil, err
	}
	return message, record, nil
}

func (p *approvalPrompt) view(width int) string {
	tools := p.form.Tools()
	noun := "tools"
	if len(tools) == 1 {
		noun = "tool"
	}
	lines := []string{theme.PromptStyle().Render(fmt.Sprintf("⏸ %s is asking permission to run %d %s", p.asker, len(tools), noun))}
	if hint := p.form.Request().Hint; hint != "" {
		lines = append(lines, wrapLines([]string{"  " + hint}, width))
	}

	nameWidth := 0
	for _, tool := range tools {
		nameWidth = max(nameWidth, ansi.StringWidth(tool.Name))
	}
	for i, tool := range tools {
		cursor := "  "
		if i == p.cursor {
			cursor = theme.PromptStyle().Render(">") + " "
		}
		pad := strings.Repeat(" ", nameWidth-ansi.StringWidth(tool.Name)+2)
		row := cursor + verdictMark(p.form.Verdict(i)) + " " + theme.HeadingStyle().Render(tool.Name)
		// cursor, "[✓] ", the name column and its gap come before the preview.
		if preview := transcript.ArgPreview(tool.Args, width-(2+4+nameWidth+2)); preview != "" {
			row += pad + theme.DimStyle().Render(preview)
		}
		lines = append(lines, ansi.Truncate(row, width, "…"))

		const indent = "      "
		switch {
		case p.reason != nil && i == p.cursor:
			p.reason.Width = max(width-len(indent)-ansi.StringWidth(p.reason.Prompt)-1, 1)
			lines = append(lines, indent+p.reason.View())
		case p.form.Verdict(i) == hitl.Rejected && p.form.Reason(i) != "":
			lines = append(lines, wrapLines([]string{indent + "reason › " + p.form.Reason(i)}, width))
		}
		if p.showArgs[i] {
			args := transcript.Render(transcript.ToolActivity{Name: tool.Name, Args: tool.Args, Outcome: transcript.AwaitingApproval{}}, width-len(indent), true, false)
			// The expanded entry's first line repeats the row; its args section is the point.
			for _, line := range strings.Split(args, "\n")[1:] {
				lines = append(lines, indent+line)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func (p *approvalPrompt) hints() string {
	if p.reason != nil {
		return "enter confirm · esc back"
	}
	return fmt.Sprintf("a approve · r reject · A approve all · space args · enter send (%d/%d) · ctrl+x discard",
		p.form.Decided(), len(p.form.Tools()))
}

func verdictMark(verdict hitl.Verdict) string {
	switch verdict {
	case hitl.Approved:
		return "[" + theme.ReadyStyle().Render("✓") + "]"
	case hitl.Rejected:
		return "[" + theme.ErrorStyle().Render("✗") + "]"
	default:
		return "[ ]"
	}
}

// wrapLines joins lines, each wrapped at width so the measured prompt height is what is drawn.
func wrapLines(lines []string, width int) string {
	wrapped := make([]string, 0, len(lines))
	for _, line := range lines {
		if width > 0 {
			line = ansi.Wrap(line, width, "")
		}
		wrapped = append(wrapped, line)
	}
	return strings.Join(wrapped, "\n")
}
