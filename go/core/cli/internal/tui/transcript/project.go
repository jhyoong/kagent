package transcript

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
)

// askUserTool is the runtime tool behind ask-user requests; its activity is the
// request's plumbing, shown instead as an AnswerRecord.
const askUserTool = "ask_user"

// ADK reports a held or refused call as an errored FunctionResponse. These are
// runtime instructions, not tool failures. Only the two complete strings
// observed on the wire match, so a real error that discusses approval still shows.
var (
	confirmationRequired = regexp.MustCompile(`^error tool "([^"]+)" requires confirmation, please approve or reject$`)
	callRejected         = regexp.MustCompile(`^error tool "([^"]+)" call is rejected$`)
)

// ClassifyResult says what a tool result means for the call that produced it.
func ClassifyResult(name string, response any) ToolOutcome {
	fields, isObject := response.(map[string]any)
	if !isObject {
		return Returned{Response: response}
	}
	if message, ok := fields["error"].(string); ok {
		if controlFlowMatch(confirmationRequired, message, name) {
			return AwaitingApproval{}
		}
		if controlFlowMatch(callRejected, message, name) {
			return NotRun{}
		}
	}
	_, hasError := fields["error"]
	return Returned{Response: response, Failed: hasError || fields["isError"] == true}
}

// controlFlowMatch requires the string to name this tool, when the result names one.
func controlFlowMatch(pattern *regexp.Regexp, message, name string) bool {
	match := pattern.FindStringSubmatch(message)
	return match != nil && (name == "" || match[1] == name)
}

// ApplyToolActivity folds one call or result into the transcript. A result
// settles the entry with its call ID; anything unpaired is appended.
func ApplyToolActivity(entries []Entry, activity kagenta2a.ToolActivity) []Entry {
	at := -1
	if activity.ID != "" {
		at = slices.IndexFunc(entries, func(entry Entry) bool {
			tool, ok := entry.(ToolActivity)
			return ok && tool.ID == activity.ID
		})
	}
	var tool ToolActivity
	if at >= 0 {
		tool = entries[at].(ToolActivity)
	} else {
		tool = ToolActivity{ID: activity.ID, Name: activity.Name, Outcome: Running{}}
	}
	if activity.Name != "" {
		tool.Name = activity.Name
	}
	switch activity.Kind {
	case kagenta2a.ToolCallKind:
		tool.Args = activity.Args
	case kagenta2a.ToolResultKind:
		tool.Outcome = ClassifyResult(tool.Name, activity.Response)
	}
	if at >= 0 {
		entries[at] = tool
		return entries
	}
	return append(entries, tool)
}

// Visible drops what the transcript holds but should not show: ask_user
// plumbing, and refusals an approval record in the same entries already
// explains. Callers pass one task or turn: call IDs repeat across turns.
func Visible(entries []Entry) []Entry {
	refusedCalls := map[string]struct{}{}
	refusedNames := map[string]struct{}{}
	for _, entry := range entries {
		record, ok := entry.(ApprovalRecord)
		if !ok {
			continue
		}
		for _, decision := range record.Decisions {
			tool, found := record.tool(decision.ID)
			switch {
			case decision.Approved || !found:
			case tool.CallID != "":
				refusedCalls[tool.CallID] = struct{}{}
			default:
				refusedNames[tool.Name] = struct{}{}
			}
		}
	}
	visible := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if tool, ok := entry.(ToolActivity); ok {
			if tool.Name == askUserTool {
				continue
			}
			_, refusedCall := refusedCalls[tool.ID]
			_, refusedName := refusedNames[tool.Name]
			if tool.Outcome == (NotRun{}) && (refusedCall && tool.ID != "" || refusedName) {
				continue
			}
		}
		visible = append(visible, entry)
	}
	return visible
}

// tool is the requested tool a decision ID refers to, when the request was in view.
func (r ApprovalRecord) tool(id string) (kagenta2a.HITLTool, bool) {
	at := slices.IndexFunc(r.Tools, func(tool kagenta2a.HITLTool) bool { return tool.ID == id })
	if at < 0 {
		return kagenta2a.HITLTool{}, false
	}
	return r.Tools[at], true
}

// toolName names a decided tool, or "" when the request was not in view.
func (r ApprovalRecord) toolName(id string) string {
	tool, _ := r.tool(id)
	return tool.Name
}

// piece is one step of a task before calls and results are paired: an entry,
// or a tool call or result that pairs by ID once everything is in order.
type piece struct {
	entry    Entry
	activity *kagenta2a.ToolActivity
}

// item is one history message or artifact, converted.
type item struct {
	pieces   []piece
	position time.Time
	// positioned is whether the source carried a timeline position.
	positioned bool
	text       string
}

func (it item) hasAskUser(kind kagenta2a.ToolActivityKind) bool {
	return slices.ContainsFunc(it.pieces, func(p piece) bool {
		return p.activity != nil && p.activity.Kind == kind && p.activity.Name == askUserTool
	})
}

// ProjectTask is what a past task shows, mirroring the web client's replay:
//
//   - A HITL request is not shown; its response becomes one record with it.
//   - A history message repeated by ID (or by parts, unnamed) is shown once.
//   - An artifact repeating history text is the same reply arriving twice,
//     unless timeline positions make each one distinct.
//   - Timeline positions order everything when every item has one; otherwise
//     answers are placed inside the ask_user round they answered.
//   - Tool calls and results pair by ID, then Visible applies.
func ProjectTask(task *a2atype.Task) []Entry {
	if task == nil {
		return nil
	}
	var (
		opening, answers, artifacts []item
		pendingApproval             *kagenta2a.ToolApprovalRequest
		pendingQuestions            = map[string]*kagenta2a.AskUserRequest{}
		taken                       = map[string]struct{}{}
		shown                       = map[string]struct{}{}
	)

	messages := slices.Clone(task.History)
	if task.Status.Message != nil {
		messages = append(messages, task.Status.Message)
	}
	completeTimeline := hasCompleteTimeline(messages, task.Artifacts)
	for _, message := range messages {
		if message == nil {
			continue
		}

		var pieces []piece
		isAnswer := false
		switch hitlType(message) {
		case kagenta2a.HITLTypeToolApprovalRequest:
			request, err := kagenta2a.ParseToolApprovalRequest(message)
			if err == nil {
				pendingApproval = request
				continue
			}
			pieces = malformed("tool approval request", err)
		case kagenta2a.HITLTypeAskUserRequest:
			request, err := kagenta2a.ParseAskUserRequest(message)
			if err == nil {
				pendingQuestions[request.ID] = request
				continue
			}
			pieces = malformed("ask-user request", err)
		case kagenta2a.HITLTypeToolApprovalResponse:
			response, err := kagenta2a.ParseToolApprovalResponse(message)
			if err != nil {
				pieces = malformed("tool approval response", err)
				break
			}
			record := ApprovalRecord{Decisions: response.Approvals}
			if pendingApproval != nil {
				record.Tools, record.AskedBy = requestedTools(pendingApproval)
			}
			pendingApproval = nil
			pieces = []piece{{entry: record}}
		case kagenta2a.HITLTypeAskUserResponse:
			isAnswer = true
			response, err := kagenta2a.ParseAskUserResponse(message)
			if err != nil {
				pieces = malformed("ask-user response", err)
				break
			}
			record := AnswerRecord{Answers: make([][]string, 0, len(response.Answers))}
			for _, answer := range response.Answers {
				record.Answers = append(record.Answers, answer.Answer)
			}
			if request := pendingQuestions[response.ID]; request != nil {
				record.Questions = request.Questions
				if request.Nested != nil {
					record.AskedBy = request.Nested.SubagentName
				}
			}
			delete(pendingQuestions, response.ID)
			pieces = []piece{{entry: record}}
		}
		if len(pieces) == 0 || isMalformed(pieces) {
			pieces = append(pieces, partPieces(message.Role, message.Parts)...)
		}
		if len(pieces) == 0 {
			continue
		}

		identity := messageIdentity(message)
		if _, dup := taken[identity]; dup {
			continue
		}
		taken[identity] = struct{}{}

		converted := newItem(pieces, message)
		shown[converted.text] = struct{}{}
		if isAnswer {
			answers = append(answers, converted)
		} else {
			opening = append(opening, converted)
		}
	}

	for _, artifact := range task.Artifacts {
		if artifact == nil {
			continue
		}
		pieces := partPieces(a2atype.MessageRoleAgent, artifact.Parts)
		if len(pieces) == 0 {
			continue
		}
		converted := newItem(pieces, artifact)
		if _, repeated := shown[converted.text]; !completeTimeline && converted.text != "" && repeated {
			continue
		}
		artifacts = append(artifacts, converted)
	}

	var ordered []item
	all := slices.Concat(opening, answers, artifacts)
	if len(all) > 0 && !slices.ContainsFunc(all, func(it item) bool { return !it.positioned }) {
		// The server-authored position is one order across history and artifacts.
		ordered = all
		slices.SortStableFunc(ordered, func(a, b item) int { return a.position.Compare(b.position) })
	} else {
		ordered = interleave(opening, answers, artifacts)
	}

	var entries []Entry
	for _, it := range ordered {
		for _, p := range it.pieces {
			if p.activity != nil {
				entries = ApplyToolActivity(entries, *p.activity)
			} else {
				entries = append(entries, p.entry)
			}
		}
	}
	return Visible(entries)
}

// hasCompleteTimeline is whether every message and artifact carries a timeline
// position, so positions alone order the task. It is decided before any
// deduplication so that an item's fate never depends on what follows it.
func hasCompleteTimeline(messages []*a2atype.Message, artifacts []*a2atype.Artifact) bool {
	for _, message := range messages {
		if message == nil {
			continue
		}
		if _, ok := kagenta2a.TimelinePosition(message); !ok {
			return false
		}
	}
	for _, artifact := range artifacts {
		if artifact == nil {
			continue
		}
		if _, ok := kagenta2a.TimelinePosition(artifact); !ok {
			return false
		}
	}
	return true
}

// interleave places each ask_user answer inside the round it answered, for
// records without timeline positions. Rounds are answered in the order asked;
// within a round the answer precedes its last ask_user result. Answers beyond
// the recognised rounds are appended rather than lost.
func interleave(opening, answers, agent []item) []item {
	ordered := slices.Clone(opening)
	unplaced := slices.Clone(answers)
	for at := 0; at < len(agent); {
		if !agent[at].hasAskUser(kagenta2a.ToolCallKind) {
			ordered = append(ordered, agent[at])
			at++
			continue
		}
		end := at + 1
		for end < len(agent) && !agent[end].hasAskUser(kagenta2a.ToolCallKind) {
			end++
		}
		round := agent[at:end]
		before := len(round)
		for index := len(round) - 1; index > 0; index-- {
			if round[index].hasAskUser(kagenta2a.ToolResultKind) {
				before = index
				break
			}
		}
		ordered = append(ordered, round[:before]...)
		if len(unplaced) > 0 {
			ordered = append(ordered, unplaced[0])
			unplaced = unplaced[1:]
		}
		ordered = append(ordered, round[before:]...)
		at = end
	}
	return append(ordered, unplaced...)
}

func newItem(pieces []piece, carrier a2atype.MetadataCarrier) item {
	position, positioned := kagenta2a.TimelinePosition(carrier)
	var text strings.Builder
	for _, p := range pieces {
		switch entry := p.entry.(type) {
		case UserMessage:
			text.WriteString(entry.Text)
		case AgentText:
			text.WriteString(entry.Text)
		}
	}
	return item{pieces: pieces, position: position, positioned: positioned, text: text.String()}
}

// partPieces converts parts in order: adjacent prose joins into one entry,
// structured output reads as the agent's prose, and tool activity pairs later.
// Other data and file parts have no transcript form yet.
func partPieces(role a2atype.MessageRole, parts a2atype.ContentParts) []piece {
	var pieces []piece
	var prose strings.Builder
	flush := func() {
		if prose.Len() == 0 {
			return
		}
		var entry Entry = AgentText{Text: prose.String()}
		if role == a2atype.MessageRoleUser {
			entry = UserMessage{Text: prose.String()}
		}
		pieces = append(pieces, piece{entry: entry})
		prose.Reset()
	}
	for _, part := range parts {
		if part == nil {
			continue
		}
		if text := part.Text(); text != "" {
			prose.WriteString(text)
			continue
		}
		if kagenta2a.IsStructuredOutputPart(part) {
			value, err := kagenta2a.StructuredOutputJSON(part)
			if err != nil {
				flush()
				pieces = append(pieces, malformed("structured output", err)...)
				continue
			}
			prose.WriteString(value)
			continue
		}
		if activity, ok := kagenta2a.ParseToolActivity(part); ok {
			flush()
			pieces = append(pieces, piece{activity: &activity})
		}
	}
	flush()
	return pieces
}

// requestedTools is what a tool approval request asked a human to decide: a
// nested request's top-level tools are the parent's continuation, not the ask.
func requestedTools(request *kagenta2a.ToolApprovalRequest) ([]kagenta2a.HITLTool, string) {
	if request.Nested != nil {
		return request.Nested.Tools, request.Nested.SubagentName
	}
	return request.Tools, ""
}

// hitlType is the HITL payload type a message declares, or "".
func hitlType(message *a2atype.Message) string {
	if !slices.Contains(message.Extensions, kagenta2a.HITLExtensionURI) {
		return ""
	}
	payload, _ := message.Metadata[kagenta2a.HITLExtensionURI].(map[string]any)
	kind, _ := payload["type"].(string)
	return kind
}

func malformed(what string, err error) []piece {
	return []piece{{entry: Banner{Kind: BannerError, Text: fmt.Sprintf("Malformed %s: %v", what, err)}}}
}

func isMalformed(pieces []piece) bool {
	banner, ok := pieces[0].entry.(Banner)
	return ok && banner.Kind == BannerError
}

// messageIdentity is the message ID, or its parts when the sender named none.
func messageIdentity(message *a2atype.Message) string {
	if message.ID != "" {
		return message.ID
	}
	encoded, err := json.Marshal(message.Parts)
	if err != nil {
		return fmt.Sprintf("%p", message)
	}
	return string(encoded)
}
