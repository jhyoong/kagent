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
		if form, ok := hitl.NewAnswerForm(pending); ok {
			return newQuestionPrompt(form)
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

// questionPrompt answers an ask-user request one question at a time, then
// shows the answers for review when there are several.
type questionPrompt struct {
	form *hitl.AnswerForm
	// cursor is the highlighted choice of the current question.
	cursor int
	// text edits the current question's answer when it is free text; nil otherwise.
	text *textinput.Model
}

func newQuestionPrompt(form *hitl.AnswerForm) *questionPrompt {
	p := &questionPrompt{form: form}
	p.enter()
	return p
}

func (p *questionPrompt) key(msg tea.KeyMsg) bool {
	if p.form.Reviewing() {
		switch msg.String() {
		case "enter":
			return p.form.Ready()
		case "ctrl+p":
			p.back()
		}
		return false
	}
	if p.text != nil {
		switch msg.String() {
		case "enter":
			p.form.SetText(p.text.Value())
			return p.advance()
		case "ctrl+p", "esc":
			p.back()
		default:
			updated, _ := p.text.Update(msg)
			p.text = &updated
		}
		return false
	}
	current := p.form.Current()
	choices := p.form.Questions()[current].Choices
	switch msg.String() {
	case "up":
		p.cursor = max(p.cursor-1, 0)
	case "down":
		p.cursor = min(p.cursor+1, len(choices)-1)
	case " ", "space":
		if p.form.Kind(current) == hitl.AnyChoices {
			p.form.Toggle(choices[p.cursor])
		} else {
			p.form.Choose(choices[p.cursor])
		}
	case "enter":
		if p.form.Kind(current) == hitl.OneChoice {
			p.form.Choose(choices[p.cursor])
		}
		return p.advance()
	case "ctrl+p":
		p.back()
	}
	return false
}

// advance sends answers that are ready, or moves past the answered question.
func (p *questionPrompt) advance() bool {
	if p.form.Ready() {
		return true
	}
	if p.form.Next() {
		p.enter()
	}
	return false
}

// back keeps the typed text and returns to the previous question or from review.
func (p *questionPrompt) back() {
	if p.text != nil {
		p.form.SetText(p.text.Value())
	}
	if p.form.Prev() {
		p.enter()
	}
}

// enter prepares the current question: the cursor on its earlier choice, or its
// earlier text in the field.
func (p *questionPrompt) enter() {
	p.cursor, p.text = 0, nil
	if p.form.Reviewing() {
		return
	}
	current := p.form.Current()
	if p.form.Kind(current) != hitl.FreeText {
		for i, choice := range p.form.Questions()[current].Choices {
			if p.form.Selected(current, choice) {
				p.cursor = i
				break
			}
		}
		return
	}
	input := textinput.New()
	input.Prompt = "› "
	input.Placeholder = "Your answer"
	if earlier := p.form.Selections(current); len(earlier) > 0 {
		input.SetValue(earlier[0])
		input.CursorEnd()
	}
	input.Focus()
	p.text = &input
}

func (p *questionPrompt) answer(contextID string) (*a2atype.Message, transcript.Entry, error) {
	message, record, err := p.form.Answer(contextID)
	if err != nil {
		return nil, nil, err
	}
	return message, record, nil
}

func (p *questionPrompt) view(width int) string {
	if p.form.Reviewing() {
		return p.reviewView(width)
	}
	questions := p.form.Questions()
	current := p.form.Current()
	question := questions[current]
	label := ""
	switch p.form.Kind(current) {
	case hitl.OneChoice:
		label = "choose one"
	case hitl.AnyChoices:
		label = "choose any"
	}
	labelled := func(text string) string {
		if label == "" {
			return text
		}
		return text + "   " + theme.DimStyle().Render(label)
	}

	var lines []string
	const indent = "  "
	if len(questions) == 1 {
		// One question is the heading itself.
		lines = append(lines, labelled(theme.PromptStyle().Render("? "+question.Question)))
	} else {
		lines = append(lines,
			theme.PromptStyle().Render("? "+p.asker()+" asked you something")+"   "+theme.DimStyle().Render(fmt.Sprintf("(%d of %d)", current+1, len(questions))),
			labelled(indent+question.Question))
	}
	lines = []string{wrapLines(lines, width)}

	if p.text != nil {
		p.text.Width = max(width-len(indent)-ansi.StringWidth(p.text.Prompt)-1, 1)
		lines = append(lines, indent+p.text.View())
		return strings.Join(lines, "\n")
	}
	for i, choice := range question.Choices {
		cursor := indent + "  "
		if i == p.cursor {
			cursor = indent + theme.PromptStyle().Render(">") + " "
		}
		lines = append(lines, ansi.Truncate(cursor+p.choiceMark(current, choice)+" "+choice, width, "…"))
	}
	return strings.Join(lines, "\n")
}

// reviewView lists every answer beside its question, numbered, before sending.
func (p *questionPrompt) reviewView(width int) string {
	questions := p.form.Questions()
	// The question column is capped so a long question leaves room for its answer.
	column := 0
	for _, question := range questions {
		column = max(column, ansi.StringWidth(question.Question))
	}
	column = min(column, max(width/2, 10))
	lines := []string{theme.PromptStyle().Render("? Review answers")}
	for i, question := range questions {
		text := ansi.Truncate(question.Question, column, "…")
		pad := strings.Repeat(" ", column-ansi.StringWidth(text)+3)
		row := fmt.Sprintf("  %d %s%s%s", i+1, text, pad, strings.Join(p.form.Selections(i), ", "))
		lines = append(lines, ansi.Truncate(row, width, "…"))
	}
	return strings.Join(lines, "\n")
}

func (p *questionPrompt) hints() string {
	if p.form.Reviewing() {
		return "enter send · ctrl+p edit · ctrl+x discard"
	}
	current := p.form.Current()
	single := len(p.form.Questions()) == 1
	var hints []string
	switch p.form.Kind(current) {
	case hitl.FreeText:
		if single {
			hints = append(hints, "enter send")
		} else {
			hints = append(hints, "enter next")
		}
		if current > 0 {
			hints = append(hints, "esc back")
		}
	case hitl.OneChoice:
		hints = append(hints, "↑↓ move")
		if single {
			hints = append(hints, "enter send")
		} else {
			hints = append(hints, "enter choose")
		}
	case hitl.AnyChoices:
		hints = append(hints, "↑↓ move", "space toggle")
		if single {
			hints = append(hints, "enter send")
		} else {
			hints = append(hints, "enter next")
		}
	}
	if current > 0 && p.form.Kind(current) != hitl.FreeText {
		hints = append(hints, "ctrl+p previous")
	}
	return strings.Join(append(hints, "ctrl+x discard"), " · ")
}

// asker names who asked; a direct request is the agent's own.
func (p *questionPrompt) asker() string {
	if askedBy := p.form.Request().AskedBy; askedBy != "" {
		return askedBy
	}
	return "The agent"
}

func (p *questionPrompt) choiceMark(question int, choice string) string {
	selected := p.form.Selected(question, choice)
	if p.form.Kind(question) == hitl.AnyChoices {
		if selected {
			return "[" + theme.ReadyStyle().Render("x") + "]"
		}
		return "[ ]"
	}
	if selected {
		return "(" + theme.ReadyStyle().Render("•") + ")"
	}
	return "( )"
}
