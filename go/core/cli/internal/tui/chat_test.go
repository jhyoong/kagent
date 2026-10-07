package tui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/hitl"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/transcript"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"trpc.group/trpc-go/trpc-a2a-go/protocol"
)

// fakeSender records sends and hands back a stream the test feeds by hand.
type fakeSender struct {
	sent    []protocol.SendMessageParams
	stopped []context.Context
	err     error
}

func (f *fakeSender) send(ctx context.Context, params protocol.SendMessageParams) (<-chan protocol.StreamingMessageEvent, error) {
	f.sent = append(f.sent, params)
	f.stopped = append(f.stopped, ctx)
	if f.err != nil {
		return nil, f.err
	}
	return make(chan protocol.StreamingMessageEvent), nil
}

func (f *fakeSender) last(t *testing.T) protocol.Message {
	t.Helper()
	require.NotEmpty(t, f.sent)
	return f.sent[len(f.sent)-1].Message
}

type fakeClipboard struct{ texts []string }

func (c *fakeClipboard) WriteClipboard(text string) error {
	c.texts = append(c.texts, text)
	return nil
}

func newTestChat(t *testing.T) (*chatModel, *fakeSender) {
	t.Helper()
	sender := &fakeSender{}
	m := newChatModel("shop/billing", "sess-1", sender.send, false)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m, sender
}

func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+x":
		return tea.KeyMsg{Type: tea.KeyCtrlX}
	case "ctrl+p":
		return tea.KeyMsg{Type: tea.KeyCtrlP}
	case "ctrl+o":
		return tea.KeyMsg{Type: tea.KeyCtrlO}
	case "ctrl+g":
		return tea.KeyMsg{Type: tea.KeyCtrlG}
	case "ctrl+y":
		return tea.KeyMsg{Type: tea.KeyCtrlY}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func press(m *chatModel, keys ...string) tea.Cmd {
	var cmd tea.Cmd
	for _, k := range keys {
		_, cmd = m.Update(keyMsg(k))
	}
	return cmd
}

func typeText(m *chatModel, text string) {
	for _, r := range text {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// deliver feeds one event of the running stream.
func deliver(m *chatModel, result protocol.StreamingMessageResult) {
	m.Update(streamMsg{gen: m.streamGen, event: protocol.StreamingMessageEvent{Result: result}})
}

func endOfStream(m *chatModel) {
	m.Update(streamDoneMsg{gen: m.streamGen})
}

func agentMessage(t *testing.T, parts ...map[string]any) *protocol.Message {
	t.Helper()
	data, err := json.Marshal(map[string]any{"kind": "message", "messageId": "m", "role": "agent", "parts": parts})
	require.NoError(t, err)
	var message protocol.Message
	require.NoError(t, json.Unmarshal(data, &message))
	return &message
}

func confirmationPart(id string, original map[string]any) map[string]any {
	return map[string]any{
		"kind": "data",
		"data": map[string]any{
			"name": "adk_request_confirmation",
			"id":   id,
			"args": map[string]any{"originalFunctionCall": original, "toolConfirmation": map[string]any{"hint": "needs approval"}},
		},
		"metadata": map[string]any{"kagent_type": "function_call", "kagent_is_long_running": true},
	}
}

func pausedStatus(t *testing.T, taskID string, parts ...map[string]any) *protocol.TaskStatusUpdateEvent {
	t.Helper()
	return &protocol.TaskStatusUpdateEvent{
		TaskID: taskID,
		Final:  true,
		Status: protocol.TaskStatus{State: protocol.TaskStateInputRequired, Message: agentMessage(t, parts...)},
	}
}

func twoToolsPaused(t *testing.T) *protocol.TaskStatusUpdateEvent {
	return pausedStatus(t, "task-1",
		confirmationPart("conf1", map[string]any{"name": "k8s_delete_pod", "id": "c1", "args": map[string]any{"pod": "p"}}),
		confirmationPart("conf2", map[string]any{"name": "k8s_scale", "id": "c2", "args": map[string]any{"replicas": 0}}),
	)
}

func questionsPaused(t *testing.T) *protocol.TaskStatusUpdateEvent {
	return pausedStatus(t, "task-1", confirmationPart("conf", map[string]any{"name": "ask_user", "id": "q", "args": map[string]any{
		"questions": []any{
			map[string]any{"question": "Which region?", "choices": []any{"eu", "us"}},
			map[string]any{"question": "Anything else?"},
		},
	}}))
}

func screen(m *chatModel) string { return ansi.Strip(m.View()) }

func decisionData(t *testing.T, message protocol.Message) map[string]any {
	t.Helper()
	decision, ok := hitl.ReadDecision(message)
	require.True(t, ok, "message carries no decision")
	_ = decision
	for _, part := range message.Parts {
		if dp, ok := part.(protocol.DataPart); ok {
			return dp.Data.(map[string]any)
		}
	}
	t.Fatal("no data part")
	return nil
}

// startTurn sends a message and leaves its stream running.
func startTurn(t *testing.T, m *chatModel, text string) {
	t.Helper()
	typeText(m, text)
	press(m, "enter")
	require.True(t, m.isStreaming())
}

func TestComposerKeepsTypedKeys(t *testing.T) {
	m, _ := newTestChat(t)
	for range 40 {
		m.log.Append(transcript.AgentText{Text: "line"})
	}
	m.render()
	offset := m.vp.YOffset
	typeText(m, "qjk fbud1")
	assert.Equal(t, "qjk fbud1", m.input.Value())
	assert.Equal(t, offset, m.vp.YOffset, "letters do not scroll the transcript")

	press(m, "pgup")
	assert.Less(t, m.vp.YOffset, offset, "page keys still scroll")

	m.input.Reset()
	for _, chunk := range []string{"clean ", "up", " now"} {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(chunk)})
	}
	assert.Equal(t, "clean up now", m.input.Value(), "a burst chunk that spells a key name is text")
}

func TestEscNeverQuits(t *testing.T) {
	m, _ := newTestChat(t)
	cmd := press(m, "esc")
	assert.Nil(t, cmd, "esc while idle does nothing")
}

func TestSendAndStreamText(t *testing.T) {
	m, sender := newTestChat(t)
	startTurn(t, m, "hello")

	sent := sender.last(t)
	require.NotNil(t, sent.ContextID)
	assert.Equal(t, "sess-1", *sent.ContextID)
	assert.Nil(t, sent.TaskID, "a new message starts a new task")
	assert.Empty(t, m.input.Value())

	partial := agentMessage(t, map[string]any{"kind": "text", "text": "Hi "})
	partial.Metadata = map[string]any{"kagent_adk_partial": true}
	deliver(m, &protocol.TaskStatusUpdateEvent{TaskID: "task-1", Status: protocol.TaskStatus{State: protocol.TaskStateWorking, Message: partial}})
	deliver(m, &protocol.TaskStatusUpdateEvent{TaskID: "task-1", Status: protocol.TaskStatus{State: protocol.TaskStateWorking, Message: agentMessage(t, map[string]any{"kind": "text", "text": "Hi there."})}})
	last := true
	deliver(m, &protocol.TaskArtifactUpdateEvent{TaskID: "task-1", LastChunk: &last, Artifact: protocol.Artifact{Parts: agentMessage(t, map[string]any{"kind": "text", "text": "Hi there."}).Parts}})
	deliver(m, &protocol.TaskStatusUpdateEvent{TaskID: "task-1", Final: true, Status: protocol.TaskStatus{State: protocol.TaskStateCompleted}})
	endOfStream(m)

	assert.Equal(t, []transcript.Entry{transcript.UserMessage{Text: "hello"}, transcript.AgentText{Text: "Hi there."}}, m.log.Entries())
	assert.False(t, m.isStreaming())
	assert.False(t, m.working)
}

func TestToolApprovalFlow(t *testing.T) {
	m, sender := newTestChat(t)
	startTurn(t, m, "clean up")
	deliver(m, twoToolsPaused(t))
	endOfStream(m)

	turn, ok := m.turn.(*awaitingTurn)
	require.True(t, ok, "the paused task's request replaces the composer")
	assert.Contains(t, screen(m), "shop/billing is asking permission to run 2 tools")
	assert.Contains(t, screen(m), `pod="p"`)

	press(m, "a")
	press(m, "enter")
	assert.Len(t, sender.sent, 1, "enter waits until every tool is decided")

	press(m, "r")
	typeText(m, "too risky")
	press(m, "enter")
	assert.Equal(t, hitl.Rejected, turn.prompt.(*approvalPrompt).form.Verdict(1))
	press(m, "enter")

	require.Len(t, sender.sent, 2)
	answer := sender.last(t)
	require.NotNil(t, answer.TaskID)
	assert.Equal(t, "task-1", *answer.TaskID)
	assert.Equal(t, "sess-1", *answer.ContextID)
	assert.Equal(t, map[string]any{
		"decision_type":     "batch",
		"decisions":         map[string]any{"c1": "approve", "c2": "reject"},
		"rejection_reasons": map[string]any{"c2": "too risky"},
	}, decisionData(t, answer))

	assert.True(t, m.isStreaming())
	entries := m.log.Entries()
	record, ok := entries[len(entries)-1].(transcript.ApprovalRecord)
	require.True(t, ok, "the decision shows as a record")
	assert.Equal(t, []bool{true, false}, record.Approved)
	assert.NotContains(t, screen(m), "You: Batch", "the decision label is not shown as prose")
}

func TestApproveAll(t *testing.T) {
	m, sender := newTestChat(t)
	startTurn(t, m, "go")
	deliver(m, twoToolsPaused(t))
	endOfStream(m)
	press(m, "A", "enter")
	assert.Equal(t, map[string]any{"decision_type": "approve"}, decisionData(t, sender.last(t)))
}

func TestQuestionFlow(t *testing.T) {
	m, sender := newTestChat(t)
	startTurn(t, m, "help")
	deliver(m, questionsPaused(t))
	endOfStream(m)
	require.IsType(t, &awaitingTurn{}, m.turn)
	assert.Contains(t, screen(m), "Which region?")

	press(m, "down", "enter") // us
	assert.Contains(t, screen(m), "Anything else?")
	press(m, "ctrl+p")
	assert.Contains(t, screen(m), "Which region?", "ctrl+p returns to the previous question")
	press(m, "enter") // keep us
	typeText(m, "no")
	press(m, "enter")
	assert.Contains(t, screen(m), "Review answers")
	press(m, "enter")

	answer := sender.last(t)
	assert.Equal(t, "task-1", *answer.TaskID)
	assert.Equal(t, map[string]any{
		"decision_type":    "approve",
		"ask_user_answers": []any{map[string]any{"answer": []any{"us"}}, map[string]any{"answer": []any{"no"}}},
	}, decisionData(t, answer))
	entries := m.log.Entries()
	assert.Equal(t, [][]string{{"us"}, {"no"}}, entries[len(entries)-1].(transcript.AnswerRecord).Answers)
}

func TestDiscardRejectsTheRequest(t *testing.T) {
	m, sender := newTestChat(t)
	startTurn(t, m, "go")
	deliver(m, twoToolsPaused(t))
	endOfStream(m)

	press(m, "ctrl+x")
	assert.Contains(t, screen(m), "Reject this request?")
	press(m, "n")
	require.IsType(t, &awaitingTurn{}, m.turn, "n keeps the request")
	assert.Len(t, sender.sent, 1)

	press(m, "ctrl+x", "y")
	require.Len(t, sender.sent, 2)
	answer := sender.last(t)
	assert.Equal(t, "task-1", *answer.TaskID)
	assert.Equal(t, map[string]any{"decision_type": "reject"}, decisionData(t, answer))
	assert.True(t, m.isStreaming(), "the agent's reaction streams")
	entries := m.log.Entries()
	assert.Equal(t, []bool{false, false}, entries[len(entries)-1].(transcript.ApprovalRecord).Approved)
}

func TestUnknownRequestCanOnlyBeRejected(t *testing.T) {
	m, sender := newTestChat(t)
	startTurn(t, m, "go")
	deliver(m, pausedStatus(t, "task-1",
		confirmationPart("c", map[string]any{"name": "ask_user", "id": "q", "args": map[string]any{"questions": []any{map[string]any{"question": "x"}}}}),
		confirmationPart("d", map[string]any{"name": "delete", "id": "c1"}),
	))
	endOfStream(m)
	assert.Contains(t, screen(m), "cannot answer")
	press(m, "enter", "a")
	assert.Len(t, sender.sent, 1)
	press(m, "ctrl+x", "y")
	assert.Equal(t, map[string]any{"decision_type": "reject"}, decisionData(t, sender.last(t)))
}

func TestFailedResumeShowsTheRequestAgain(t *testing.T) {
	m, sender := newTestChat(t)
	startTurn(t, m, "go")
	deliver(m, twoToolsPaused(t))
	endOfStream(m)
	sender.err = errors.New("connection refused")
	press(m, "A", "enter")
	require.IsType(t, &awaitingTurn{}, m.turn)
	assert.Contains(t, screen(m), "connection refused")
}

func TestDoubleEscStopsListening(t *testing.T) {
	m, sender := newTestChat(t)
	startTurn(t, m, "go")
	ctx := sender.stopped[0]

	cmd := press(m, "esc")
	assert.NotNil(t, cmd, "the first esc arms and schedules the disarm")
	assert.True(t, m.isStreaming())
	assert.Contains(t, screen(m), "press esc again to stop listening")

	press(m, "x")
	assert.NotContains(t, screen(m), "press esc again", "another key disarms")
	press(m, "esc")
	m.Update(stopDisarmMsg{armedAt: m.turn.(*streamingTurn).stopArmedAt})
	assert.NotContains(t, screen(m), "press esc again", "the window closes")

	gen := m.streamGen
	cmd = press(m, "esc", "esc")
	assert.Nil(t, cmd, "stopping does not quit")
	assert.False(t, m.isStreaming())
	assert.Error(t, ctx.Err(), "the connection is dropped")
	assert.Contains(t, screen(m), "Stopped listening")

	before := len(m.log.Entries())
	m.Update(streamMsg{gen: gen, event: protocol.StreamingMessageEvent{Result: agentMessage(t, map[string]any{"kind": "text", "text": "late"})}})
	assert.Len(t, m.log.Entries(), before, "events of the abandoned stream are ignored")
}

func TestHistoryRestoresPendingRequest(t *testing.T) {
	m, sender := newTestChat(t)
	m.historyPending = true
	typeText(m, "hi")
	press(m, "enter")
	assert.Empty(t, sender.sent, "no send before history arrives")

	paused := &protocol.Task{
		ID:     "task-9",
		Status: protocol.TaskStatus{State: protocol.TaskStateInputRequired, Message: twoToolsPaused(t).Status.Message},
		History: []protocol.Message{
			*agentMessage(t, map[string]any{"kind": "text", "text": "earlier"}),
		},
	}
	m.loadHistory([]*protocol.Task{paused}, nil)
	require.IsType(t, &awaitingTurn{}, m.turn)
	press(m, "A", "enter")
	assert.Equal(t, "task-9", *sender.last(t).TaskID)
}

func TestHistoryNotesARunningTurn(t *testing.T) {
	m, _ := newTestChat(t)
	m.loadHistory([]*protocol.Task{{ID: "t", Status: protocol.TaskStatus{State: protocol.TaskStateWorking}}}, nil)
	assert.Contains(t, screen(m), "The last turn is still running")
	assert.IsType(t, idleTurn{}, m.turn)
}

func TestHistoryError(t *testing.T) {
	m, _ := newTestChat(t)
	m.historyPending = true
	m.loadHistory(nil, errors.New("boom"))
	assert.False(t, m.historyPending)
	assert.Contains(t, screen(m), "Could not load history: boom")
}

func TestFoldingAndCopy(t *testing.T) {
	m, _ := newTestChat(t)
	clip := &fakeClipboard{}
	m.clip = clip
	m.log.AddUserMessage("q")
	m.log.Append(transcript.ToolActivity{ID: "c1", Name: "k8s_get_pods", Args: map[string]any{"ns": "shop"}, Outcome: transcript.Returned{Response: map[string]any{"result": "pod-a"}}})
	m.log.Append(transcript.AgentText{Text: "The answer."})
	m.render()
	assert.NotContains(t, screen(m), "pod-a", "tool output starts collapsed")

	press(m, "ctrl+o")
	assert.Contains(t, screen(m), "pod-a")
	press(m, "ctrl+o")
	assert.NotContains(t, screen(m), "pod-a")

	press(m, "ctrl+g")
	assert.Equal(t, modeSelect, m.mode)
	press(m, "k", "enter")
	assert.Contains(t, screen(m), "pod-a", "enter toggles the selected entry")
	press(m, "y")
	require.Len(t, clip.texts, 1)
	assert.True(t, strings.Contains(clip.texts[0], "pod-a"))
	press(m, "Y")
	assert.Contains(t, clip.texts[1], "You: q")
	press(m, "esc")
	assert.Equal(t, modeCompose, m.mode)

	press(m, "ctrl+y")
	assert.Equal(t, "The answer.", clip.texts[2])
	assert.Contains(t, screen(m), "copied 11 bytes")
}

func TestFailedTaskBanner(t *testing.T) {
	m, _ := newTestChat(t)
	startTurn(t, m, "go")
	deliver(m, &protocol.TaskStatusUpdateEvent{TaskID: "t", Final: true, Status: protocol.TaskStatus{
		State: protocol.TaskStateFailed, Message: agentMessage(t, map[string]any{"kind": "text", "text": "LLM error"}),
	}})
	endOfStream(m)
	assert.Contains(t, screen(m), "✗ Task failed. LLM error")
	assert.IsType(t, idleTurn{}, m.turn)
}
