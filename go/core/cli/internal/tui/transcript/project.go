package transcript

import (
	"strings"

	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/hitl"
	"trpc.group/trpc-go/trpc-a2a-go/protocol"
)

// Tool calls that are runtime control flow rather than the agent's own tools.
// Confirmations become records; ask_user is shown by its prompt and answer record.
var hiddenCalls = map[string]bool{
	"adk_request_confirmation": true,
	"adk_request_credential":   true,
	"ask_user":                 true,
}

// rejectedPrefix opens the result the runtimes return for a call a human rejected.
const rejectedPrefix = "Tool call was rejected by user."

// Log is a conversation as it is shown, built from task history and stream
// events. Tool results pair with their calls by ID within the current turn;
// call IDs may repeat across turns.
type Log struct {
	entries []Entry
	// turnStart is where the current turn's entries begin.
	turnStart int
	// open is the index of the AgentText being streamed from partial chunks; -1 when none.
	open int
	// seen holds history message IDs already projected, so a message repeated
	// across tasks shows once.
	seen map[string]struct{}
}

// NewLog is an empty conversation.
func NewLog() *Log { return &Log{open: -1} }

// Entries is the conversation in order. The slice is the log's; callers do not modify it.
func (l *Log) Entries() []Entry { return l.entries }

// Append adds an entry after everything shown, ending any streamed text.
func (l *Log) Append(entry Entry) {
	l.open = -1
	l.entries = append(l.entries, entry)
}

// StartTurn begins a new turn: earlier tool calls no longer pair with results.
func (l *Log) StartTurn() {
	l.open = -1
	l.turnStart = len(l.entries)
}

// AddUserMessage starts a turn with prose the reader sent.
func (l *Log) AddUserMessage(text string) {
	l.StartTurn()
	l.Append(UserMessage{Text: text})
}

// ApplyMessage adds what an agent message shows: its text and its tool activity,
// in part order. A partial message is a streaming chunk; the next whole message
// replaces the chunks it repeats.
func (l *Log) ApplyMessage(message protocol.Message, partial bool) {
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			l.addText(text.String(), partial)
			text.Reset()
		}
	}
	for _, part := range message.Parts {
		if t, ok := asTextPart(part); ok {
			text.WriteString(t.Text)
			continue
		}
		data := asDataPart(part)
		if data == nil {
			continue
		}
		fields, ok := data.Data.(map[string]any)
		if !ok {
			continue
		}
		switch hitl.MetadataValue(data.Metadata, "type") {
		case "function_call":
			flush()
			l.applyCall(fields)
		case "function_response":
			flush()
			l.applyResponse(fields)
		}
	}
	flush()
}

// ApplyArtifact adds a final artifact's text unless it repeats the turn's last
// text: both runtimes echo the final message as an artifact.
func (l *Log) ApplyArtifact(parts []protocol.Part) {
	text := strings.TrimSpace(TextOf(parts))
	if text == "" {
		return
	}
	for i := len(l.entries) - 1; i >= l.turnStart; i-- {
		if previous, ok := l.entries[i].(AgentText); ok {
			if strings.TrimSpace(previous.Text) == text {
				l.open = -1
				return
			}
			break
		}
	}
	l.Append(AgentText{Text: text})
}

// ApplyTask projects a past task's history. A request answered later in the
// history becomes its record; a request still pending is left out, as it is
// shown by the prompt. Decision messages are shown by their records.
func (l *Log) ApplyTask(task *protocol.Task) {
	if task == nil {
		return
	}
	if l.seen == nil {
		l.seen = map[string]struct{}{}
	}
	for i, message := range task.History {
		if message.MessageID != "" {
			if _, ok := l.seen[message.MessageID]; ok {
				continue
			}
			l.seen[message.MessageID] = struct{}{}
		}
		if request, ok := hitl.ReadRequest(message.Parts); ok {
			decision, answered := decisionAfter(task.History[i+1:])
			if !answered {
				continue
			}
			if record, ok := RecordFor(request, decision); ok {
				l.Append(record)
			} else {
				l.Append(Banner{Kind: BannerInfo, Text: "The request was declined."})
			}
			continue
		}
		if message.Role == protocol.MessageRoleUser {
			if hitl.IsDecision(message) {
				continue
			}
			if text := strings.TrimSpace(TextOf(message.Parts)); text != "" {
				l.AddUserMessage(text)
			}
			continue
		}
		l.ApplyMessage(message, false)
	}
	l.open = -1
}

// decisionAfter is the first decision a user sent in history.
func decisionAfter(history []protocol.Message) (hitl.Decision, bool) {
	for _, message := range history {
		if message.Role != protocol.MessageRoleUser {
			continue
		}
		if decision, ok := hitl.ReadDecision(message); ok {
			return decision, true
		}
	}
	return hitl.Decision{}, false
}

// addText adds agent prose. A chunk extends the streamed entry; whole text
// replaces the chunks streamed before it, or starts a new entry.
func (l *Log) addText(text string, partial bool) {
	if l.open >= 0 {
		i := l.open
		entry := l.entries[i].(AgentText)
		if partial {
			entry.Text += text
		} else {
			entry.Text = text
			l.open = -1
		}
		l.entries[i] = entry
		return
	}
	if !partial && strings.TrimSpace(text) == "" {
		return
	}
	l.entries = append(l.entries, AgentText{Text: text})
	if partial {
		l.open = len(l.entries) - 1
	}
}

func (l *Log) applyCall(fields map[string]any) {
	name, _ := fields["name"].(string)
	if hiddenCalls[name] {
		return
	}
	id, _ := fields["id"].(string)
	if i := l.findTool(id); i >= 0 {
		tool := l.entries[i].(ToolActivity)
		tool.Name, tool.Args = name, fields["args"]
		l.entries[i] = tool
		return
	}
	l.Append(ToolActivity{ID: id, Name: name, Args: fields["args"], Outcome: Running{}})
}

func (l *Log) applyResponse(fields map[string]any) {
	name, _ := fields["name"].(string)
	if hiddenCalls[name] {
		return
	}
	id, _ := fields["id"].(string)
	outcome := ClassifyResult(fields["response"])
	if i := l.findTool(id); i >= 0 {
		tool := l.entries[i].(ToolActivity)
		tool.Outcome = outcome
		if tool.Name == "" {
			tool.Name = name
		}
		l.entries[i] = tool
		return
	}
	l.Append(ToolActivity{ID: id, Name: name, Outcome: outcome})
}

// findTool is the index of the current turn's tool activity with id; -1 when none.
func (l *Log) findTool(id string) int {
	if id == "" {
		return -1
	}
	for i := len(l.entries) - 1; i >= l.turnStart; i-- {
		if tool, ok := l.entries[i].(ToolActivity); ok && tool.ID == id {
			return i
		}
	}
	return -1
}

// ClassifyResult is what a tool response says happened. The runtimes answer a
// call held for approval with a "confirmation_requested" or "pending" status,
// and a rejected call with a fixed sentence.
func ClassifyResult(response any) ToolOutcome {
	fields, _ := response.(map[string]any)
	switch fields["status"] {
	case "confirmation_requested", "pending":
		return AwaitingApproval{}
	}
	if result, _ := fields["result"].(string); strings.HasPrefix(result, rejectedPrefix) {
		return NotRun{}
	}
	if text, _ := response.(string); strings.HasPrefix(text, rejectedPrefix) {
		return NotRun{}
	}
	failed := fields["isError"] == true || fields["is_error"] == true
	return Returned{Response: response, Failed: failed}
}

// IsPartial reports whether metadata marks a streaming chunk; runtimes spell the key three ways.
func IsPartial(metadata map[string]any) bool {
	for _, key := range []string{"kagent_adk_partial", "adk_partial", "kagent_partial"} {
		if partial, ok := metadata[key].(bool); ok && partial {
			return true
		}
	}
	return false
}

// TextOf joins the text parts of parts.
func TextOf(parts []protocol.Part) string {
	var text strings.Builder
	for _, part := range parts {
		if t, ok := asTextPart(part); ok {
			text.WriteString(t.Text)
		}
	}
	return text.String()
}

func asTextPart(part protocol.Part) (protocol.TextPart, bool) {
	switch p := part.(type) {
	case *protocol.TextPart:
		return *p, true
	case protocol.TextPart:
		return p, true
	}
	return protocol.TextPart{}, false
}

func asDataPart(part protocol.Part) *protocol.DataPart {
	switch p := part.(type) {
	case *protocol.DataPart:
		return p
	case protocol.DataPart:
		return &p
	}
	return nil
}
