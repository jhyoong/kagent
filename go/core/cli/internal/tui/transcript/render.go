package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/theme"
)

// maxExpandedLines caps each section of an expanded tool so one large result
// cannot bury the conversation; PlainText keeps the whole of it.
const maxExpandedLines = 40

// Render draws one entry at width columns, with no left gutter. Width <= 0
// means unbounded. Only tool activity has an expanded form; selected marks the
// entry's first line without changing its text.
func Render(e Entry, width int, expanded, selected bool) string {
	var lines []string
	switch e := e.(type) {
	case UserMessage:
		lines = wrap(theme.UserStyle().Render("You:")+" "+e.Text, width)
	case AgentText:
		lines = append([]string{theme.AgentStyle().Render("Agent:")}, wrap(e.Text, width)...)
	case Banner:
		lines = styleLines(bannerStyle(e.Kind), wrap(e.Text, width))
	case ApprovalRecord:
		lines = wrap(approvalLine(e, true), width)
	case AnswerRecord:
		for _, line := range answerLines(e) {
			lines = append(lines, wrap(line, width)...)
		}
	case ToolActivity:
		if expanded {
			lines = expandedTool(e, width)
		} else {
			lines = []string{collapsedTool(e, width)}
		}
	}
	if selected && len(lines) > 0 {
		lines[0] = selectedStyle().Render(ansi.Strip(lines[0]))
	}
	return strings.Join(lines, "\n")
}

// PlainText is an entry as copied or exported: unstyled, unwrapped, and complete.
func PlainText(e Entry) string {
	switch e := e.(type) {
	case UserMessage:
		return "You: " + e.Text
	case AgentText:
		return "Agent:\n" + e.Text
	case Banner:
		return e.Text
	case ApprovalRecord:
		return approvalLine(e, false)
	case AnswerRecord:
		return strings.Join(answerLines(e), "\n")
	case ToolActivity:
		header := outcomeIcon(e.Outcome) + " " + e.Name
		if e.ID != "" {
			header += "  " + e.ID
		}
		if status := outcomeStatus(e.Outcome); status != "" {
			header += "  " + status
		}
		lines := []string{header}
		for _, section := range toolSections(e) {
			lines = append(lines, section.title, section.body)
		}
		return strings.Join(lines, "\n")
	}
	return ""
}

// ArgPreview is a one-line summary of tool arguments, `k="v", k2=3`, sorted by
// key and truncated to width.
func ArgPreview(args any, width int) string {
	if args == nil || width <= 0 {
		return ""
	}
	preview := compactJSON(args)
	if fields, ok := args.(map[string]any); ok {
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		pairs := make([]string, 0, len(keys))
		for _, key := range keys {
			pairs = append(pairs, key+"="+compactJSON(fields[key]))
		}
		preview = strings.Join(pairs, ", ")
	}
	return ansi.Truncate(preview, width, "…")
}

// collapsedTool is `▸ ✓ name  argpreview  status`, one line with the status
// right-aligned to width.
func collapsedTool(e ToolActivity, width int) string {
	head := "▸ " + outcomeStyle(e.Outcome).Render(outcomeIcon(e.Outcome)) + " " + theme.HeadingStyle().Render(e.Name)
	tail := outcomeStatus(e.Outcome)
	if returned, ok := e.Outcome.(Returned); ok {
		tail = byteSize(len(resultBody(returned.Response)))
	}

	previewWidth := width - ansi.StringWidth(head) - 2
	if tail != "" {
		previewWidth -= ansi.StringWidth(tail) + 2
	}
	if width <= 0 {
		previewWidth = math.MaxInt
	}
	line := head
	if preview := ArgPreview(e.Args, previewWidth); preview != "" {
		line += "  " + theme.DimStyle().Render(preview)
	}
	if tail != "" {
		gap := max(width-ansi.StringWidth(line)-ansi.StringWidth(tail), 2)
		line += strings.Repeat(" ", gap) + theme.DimStyle().Render(tail)
	}
	if width > 0 {
		line = ansi.Truncate(line, width, "…")
	}
	return line
}

// expandedTool is the header, then the args and result in full up to the cap.
func expandedTool(e ToolActivity, width int) []string {
	header := "▾ " + outcomeStyle(e.Outcome).Render(outcomeIcon(e.Outcome)) + " " + theme.HeadingStyle().Render(e.Name)
	if e.ID != "" {
		header += "  " + theme.DimStyle().Render(e.ID)
	}
	if status := outcomeStatus(e.Outcome); status != "" {
		header += "  " + theme.DimStyle().Render(status)
	}
	if width > 0 {
		header = ansi.Truncate(header, width, "…")
	}
	lines := []string{header}
	for _, section := range toolSections(e) {
		lines = append(lines, theme.DimStyle().Render(section.title))
		body := strings.Split(section.body, "\n")
		hidden := len(body) - maxExpandedLines
		for _, line := range body[:min(len(body), maxExpandedLines)] {
			lines = append(lines, wrap(line, width)...)
		}
		if hidden > 0 {
			lines = append(lines, theme.DimStyle().Render(fmt.Sprintf("… %d more lines (y copies the full output)", hidden)))
		}
	}
	return lines
}

type toolSection struct{ title, body string }

func toolSections(e ToolActivity) []toolSection {
	var sections []toolSection
	if e.Args != nil {
		sections = append(sections, toolSection{"args", indentJSON(e.Args)})
	}
	if returned, ok := e.Outcome.(Returned); ok {
		sections = append(sections, toolSection{"result", resultBody(returned.Response)})
	}
	return sections
}

// resultBody is what a tool returned, as a reader wants it: a string output as
// text, anything else as JSON. `output` is what the controller sends and
// `result` another spelling seen in the wild; an unrecognised shape prints whole.
func resultBody(response any) string {
	payload := response
	if fields, ok := response.(map[string]any); ok {
		if output, ok := fields["output"]; ok && output != nil {
			payload = output
		} else if result, ok := fields["result"]; ok && result != nil {
			payload = result
		}
	}
	if text, ok := payload.(string); ok {
		return strings.TrimRight(text, "\n")
	}
	if payload == nil {
		return ""
	}
	return indentJSON(payload)
}

func outcomeIcon(outcome ToolOutcome) string {
	switch outcome := outcome.(type) {
	case Returned:
		if outcome.Failed {
			return "✗"
		}
		return "✓"
	case AwaitingApproval:
		return "⏸"
	case NotRun:
		return "⊘"
	default:
		return "…"
	}
}

func outcomeStatus(outcome ToolOutcome) string {
	switch outcome.(type) {
	case Returned:
		return ""
	case AwaitingApproval:
		return "awaiting approval"
	case NotRun:
		return "not run"
	default:
		return "running"
	}
}

func outcomeStyle(outcome ToolOutcome) lipgloss.Style {
	switch outcome := outcome.(type) {
	case Returned:
		if outcome.Failed {
			return theme.ErrorStyle()
		}
		return lipgloss.NewStyle().Foreground(theme.ColorReady)
	default:
		return theme.DimStyle()
	}
}

// approvalLine is `✓ Approved a · ✗ Rejected b: "why"  (asker)`.
func approvalLine(r ApprovalRecord, styled bool) string {
	paint := func(style lipgloss.Style, s string) string {
		if styled {
			return style.Render(s)
		}
		return s
	}
	decisions := make([]string, 0, len(r.Decisions))
	for _, decision := range r.Decisions {
		name := r.toolName(decision.ID)
		if name == "" {
			name = "tool"
		}
		if decision.Approved {
			decisions = append(decisions, paint(lipgloss.NewStyle().Foreground(theme.ColorReady), "✓ Approved")+" "+name)
			continue
		}
		rejected := paint(theme.ErrorStyle(), "✗ Rejected") + " " + name
		if decision.RejectionReason != "" {
			rejected += fmt.Sprintf(": %q", decision.RejectionReason)
		}
		decisions = append(decisions, rejected)
	}
	line := strings.Join(decisions, " · ")
	if r.AskedBy != "" {
		line += "  " + paint(theme.DimStyle(), "("+r.AskedBy+")")
	}
	return line
}

// answerLines is one `? question → answer` line per answer, unstyled.
func answerLines(r AnswerRecord) []string {
	lines := make([]string, 0, len(r.Answers))
	for i, answer := range r.Answers {
		line := "→ " + strings.Join(answer, ", ")
		if i < len(r.Questions) && r.Questions[i].Question != "" {
			line = "? " + r.Questions[i].Question + " " + line
		}
		if i == 0 && r.AskedBy != "" {
			line += "  (" + r.AskedBy + ")"
		}
		lines = append(lines, line)
	}
	return lines
}

func bannerStyle(kind BannerKind) lipgloss.Style {
	if kind == BannerError {
		return theme.ErrorStyle()
	}
	return theme.StatusStyle()
}

func selectedStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(theme.ColorSelected).Bold(true).Reverse(true)
}

// wrap breaks s at width columns; ANSI-aware, and width <= 0 leaves it whole.
func wrap(s string, width int) []string {
	if width > 0 {
		s = ansi.Wrap(s, width, "")
	}
	return strings.Split(s, "\n")
}

// styleLines styles each line on its own, so a multi-line block is not padded
// to its widest line.
func styleLines(style lipgloss.Style, lines []string) []string {
	styled := make([]string, len(lines))
	for i, line := range lines {
		styled[i] = style.Render(line)
	}
	return styled
}

func byteSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

func compactJSON(value any) string {
	return encodeJSON(value, "")
}

func indentJSON(value any) string {
	return encodeJSON(value, "  ")
}

// encodeJSON keeps <, > and & literal; a value JSON cannot encode prints with %v.
func encodeJSON(value any, indent string) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", indent)
	if err := encoder.Encode(value); err != nil {
		return fmt.Sprintf("%v", value)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}
