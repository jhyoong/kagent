package tui

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	tea "github.com/charmbracelet/bubbletea"
	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
	clia2a "github.com/kagent-dev/kagent/go/core/cli/internal/a2a"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/transcript"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTurnClient records what a chat asks of the session; its stream ends at once,
// so tests feed events through Update or streamEvent.
type fakeTurnClient struct {
	streamCtx context.Context
	sent      []*a2atype.SendMessageRequest
	cancels   []a2atype.TaskID
	cancelErr error
	// cancelResult is the task CancelTask returns; nil means canceled.
	cancelResult *a2atype.Task
}

func (f *fakeTurnClient) SendStreamingMessage(ctx context.Context, req *a2atype.SendMessageRequest) iter.Seq2[a2atype.Event, error] {
	f.streamCtx = ctx
	f.sent = append(f.sent, req)
	return func(func(a2atype.Event, error) bool) {}
}

func (f *fakeTurnClient) CancelTask(_ context.Context, req *a2atype.CancelTaskRequest) (*a2atype.Task, error) {
	f.cancels = append(f.cancels, req.ID)
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	if f.cancelResult != nil {
		return f.cancelResult, nil
	}
	return &a2atype.Task{ID: req.ID, Status: a2atype.TaskStatus{State: a2atype.TaskStateCanceled}}, nil
}

func newTestChatModel() *chatModel {
	return newChatModel(context.Background(), "reporter", "ctx-1", &fakeTurnClient{}, false)
}

// streamEvent applies an event to the running turn, starting one if the chat is idle.
func streamEvent(m *chatModel, ev a2atype.Event) {
	if !m.isStreaming() {
		m.startStream(a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("hi")))
	}
	m.appendEvent(ev)
}

// deliver hands the running stream one result, as its waitNext would.
func deliver(m *chatModel, result clia2a.StreamResult) tea.Cmd {
	var gen uint64
	if turn, ok := m.turn.(*streamingTurn); ok {
		gen = turn.gen
	}
	_, cmd := m.Update(streamMsg{gen: gen, result: result})
	return cmd
}

// shownText is the visible transcript as plain text, including the block being assembled.
func shownText(m *chatModel) string {
	visible := m.visibleEntries()
	blocks := make([]string, 0, len(visible))
	for _, entry := range visible {
		blocks = append(blocks, transcript.PlainText(entry))
	}
	return strings.Join(blocks, "\n\n")
}

func reqCtx() *a2asrv.ExecutorContext {
	return &a2asrv.ExecutorContext{TaskID: "task-1", ContextID: "ctx-1"}
}

func dataPart(kind, name string, payload map[string]any) *a2atype.Part {
	payload["name"] = name
	payload["id"] = "call-1"
	part := a2atype.NewDataPart(payload)
	part.Metadata = map[string]any{kagenta2a.PartTypeMetadataKey: kind}
	return part
}

// The projection is cumulative, so a delta must not repeat and a replacement must not concatenate.
func TestChatModelStreamsAssembledText(t *testing.T) {
	tests := []struct {
		name       string
		events     func() []a2atype.Event
		want       string
		wantAbsent string
	}{
		{
			name: "a delta extends in place",
			events: func() []a2atype.Event {
				first := a2atype.NewArtifactEvent(reqCtx(), a2atype.NewTextPart("hel"))
				return []a2atype.Event{first, a2atype.NewArtifactUpdateEvent(reqCtx(), first.Artifact.ID, a2atype.NewTextPart("lo"))}
			},
			want: "hello", wantAbsent: "helhello",
		},
		{
			name: "a replacement chunk replaces",
			events: func() []a2atype.Event {
				partial := a2atype.NewArtifactEvent(reqCtx(), a2atype.NewTextPart("hel"))
				final := a2atype.NewArtifactUpdateEvent(reqCtx(), partial.Artifact.ID, a2atype.NewTextPart("hello"))
				final.Append, final.LastChunk = false, true
				return []a2atype.Event{partial, final}
			},
			want: "hello", wantAbsent: "helhello",
		},
		{
			// Only artifacts carry output; status text is control-plane content.
			name: "a task snapshot reads artifacts, not status text",
			events: func() []a2atype.Event {
				return []a2atype.Event{&a2atype.Task{
					ID: "task-1", ContextID: "ctx-1",
					Status: a2atype.TaskStatus{
						State:   a2atype.TaskStateCompleted,
						Message: a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("control plane note")),
					},
					Artifacts: []*a2atype.Artifact{{
						ID: "artifact-1", Parts: a2atype.ContentParts{a2atype.NewTextPart("artifact result")},
					}},
				}}
			},
			want: "artifact result", wantAbsent: "control plane note",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := newTestChatModel()
			for _, event := range tt.events() {
				streamEvent(model, event)
			}

			assert.Contains(t, shownText(model), tt.want)
			assert.NotContains(t, shownText(model), tt.wantAbsent)
		})
	}
}

// Paused, terminal-failure, and transport problems must read as three different things.
func TestChatModelRendersStateClasses(t *testing.T) {
	tests := []struct {
		name       string
		apply      func(*chatModel)
		want       string
		wantAbsent string
		// stillWorking: the stream has not settled the task, so the turn keeps running.
		stillWorking bool
	}{
		{
			// The prompt replaces the composer; the transcript gets no banner.
			name: "input required is paused",
			apply: func(m *chatModel) {
				streamEvent(m, a2atype.NewStatusUpdateEvent(reqCtx(), a2atype.TaskStateInputRequired, nil))
			},
			wantAbsent: "Input required",
		},
		{
			name: "auth required is paused",
			apply: func(m *chatModel) {
				streamEvent(m, a2atype.NewStatusUpdateEvent(reqCtx(), a2atype.TaskStateAuthRequired, nil))
			},
			want: "Authentication required", wantAbsent: "✗",
		},
		{
			name: "failed is an error and keeps its explanation",
			apply: func(m *chatModel) {
				message := a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("execution failed"))
				streamEvent(m, a2atype.NewStatusUpdateEvent(reqCtx(), a2atype.TaskStateFailed, message))
			},
			want: "execution failed",
		},
		{
			name: "completed needs no banner",
			apply: func(m *chatModel) {
				streamEvent(m, a2atype.NewStatusUpdateEvent(reqCtx(), a2atype.TaskStateCompleted, nil))
			},
			wantAbsent: "✗",
		},
		{
			name: "a transport failure is not a task failure",
			apply: func(m *chatModel) {
				m.submit("hi")
				deliver(m, clia2a.StreamResult{Err: errors.New("stream disconnected")})
			},
			want: "Connection error: stream disconnected", wantAbsent: "✗ Task",
		},
		{
			// A malformed stream is neither a task nor a transport failure, and must not be dropped.
			name: "a malformed stream is a protocol error",
			apply: func(m *chatModel) {
				streamEvent(m, a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("first")))
				streamEvent(m, a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("second")))
			},
			want: "Protocol error", stillWorking: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := newTestChatModel()
			tt.apply(model)

			if tt.want != "" {
				assert.Contains(t, shownText(model), tt.want)
			}
			if tt.wantAbsent != "" {
				assert.NotContains(t, shownText(model), tt.wantAbsent)
			}
			assert.Equal(t, tt.stillWorking, model.working, "a settled task stops the working indicator")
		})
	}
}

func TestChatModelRendersToolActivityBeforeLastChunk(t *testing.T) {
	model := newTestChatModel()
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 20})

	streamEvent(model, a2atype.NewArtifactEvent(reqCtx(), a2atype.NewTextPart("checking")))
	streamEvent(model, a2atype.NewArtifactEvent(reqCtx(),
		dataPart("function_call", "get_pods", map[string]any{"args": map[string]any{"namespace": "default"}})))
	streamEvent(model, a2atype.NewArtifactEvent(reqCtx(),
		dataPart("function_response", "get_pods", map[string]any{"response": map[string]any{"pods": []any{"pod-a"}}})))

	assert.Equal(t, []transcript.Entry{
		transcript.AgentText{Text: "checking"},
		transcript.ToolActivity{
			ID: "call-1", Name: "get_pods",
			Args:    map[string]any{"namespace": "default"},
			Outcome: transcript.Returned{Response: map[string]any{"pods": []any{"pod-a"}}},
		},
	}, model.entries, "the result settles its call in place")
	assert.Contains(t, model.vp.View(), "▸ ✓ get_pods", "collapsed by default")
}

// A tool entry closes the text block, so text after it is a new block rather than an edit above it.
func TestChatModelTextAfterToolActivityIsANewBlock(t *testing.T) {
	model := newTestChatModel()

	first := a2atype.NewArtifactEvent(reqCtx(), a2atype.NewTextPart("before"))
	streamEvent(model, first)
	streamEvent(model, a2atype.NewArtifactEvent(reqCtx(), dataPart("function_call", "get_pods", map[string]any{})))
	streamEvent(model, a2atype.NewArtifactUpdateEvent(reqCtx(), first.Artifact.ID, a2atype.NewTextPart(" after")))

	assert.Equal(t, []transcript.Entry{
		transcript.AgentText{Text: "before"},
		transcript.ToolActivity{ID: "call-1", Name: "get_pods", Outcome: transcript.Running{}},
		transcript.AgentText{Text: " after"},
	}, model.entries)
}

func TestChatModelAppendsHistoryTask(t *testing.T) {
	model := newTestChatModel()

	model.appendHistoryTask(&a2atype.Task{
		ID: "task-1", ContextID: "ctx-1",
		Status: a2atype.TaskStatus{State: a2atype.TaskStateCompleted},
		History: []*a2atype.Message{
			a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("what pods?")),
			a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("There is one pod.")),
		},
		Artifacts: []*a2atype.Artifact{
			{ID: "call", Parts: a2atype.ContentParts{dataPart("function_call", "get_pods", map[string]any{"args": map[string]any{}})}},
			{ID: "result", Parts: a2atype.ContentParts{dataPart("function_response", "get_pods", map[string]any{"response": "pod-a"})}},
			{ID: "reply", Parts: a2atype.ContentParts{a2atype.NewTextPart("There is one pod.")}},
		},
	})

	got := shownText(model)
	assert.Contains(t, got, "what pods?")
	assert.Contains(t, got, "✓ get_pods", "history shows tool activity")
	assert.Equal(t, 1, strings.Count(got, "There is one pod."), "an artifact repeating history text is one reply")
}

func historyTask(id string, history []*a2atype.Message, artifacts ...*a2atype.Artifact) *a2atype.Task {
	return &a2atype.Task{
		ID: a2atype.TaskID(id), ContextID: "ctx-1",
		Status:    a2atype.TaskStatus{State: a2atype.TaskStateCompleted},
		History:   history,
		Artifacts: artifacts,
	}
}

// Models reuse call IDs across turns; a new turn's call must not update an older entry.
func TestChatModelRepeatedCallIDStartsANewEntry(t *testing.T) {
	model := newTestChatModel()
	model.appendHistoryTask(historyTask("task-0", nil,
		&a2atype.Artifact{ID: "call", Parts: a2atype.ContentParts{dataPart("function_call", "get_pods", map[string]any{"args": map[string]any{}})}},
		&a2atype.Artifact{ID: "result", Parts: a2atype.ContentParts{dataPart("function_response", "get_pods", map[string]any{"response": "old"})}},
	))
	old := model.entries[0]

	model.appendUser("again")
	model.submit("again")
	streamEvent(model, a2atype.NewArtifactEvent(reqCtx(), a2atype.NewTextPart("checking")))
	streamEvent(model, a2atype.NewArtifactEvent(reqCtx(), dataPart("function_call", "get_pods", map[string]any{})))
	streamEvent(model, a2atype.NewArtifactEvent(reqCtx(), dataPart("function_response", "get_pods", map[string]any{"response": "new"})))

	assert.Equal(t, []transcript.Entry{
		old,
		transcript.UserMessage{Text: "again"},
		transcript.AgentText{Text: "checking"},
		transcript.ToolActivity{ID: "call-1", Name: "get_pods", Outcome: transcript.Returned{Response: "new"}},
	}, model.entries)
	assert.False(t, model.textOpen, "the new tool entry closes the text block")
}

// A rejection recorded in an earlier task does not explain a later refusal.
func TestChatModelEarlierRejectionDoesNotHideLaterNotRun(t *testing.T) {
	model := newTestChatModel()
	request := a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("approve?"))
	require.NoError(t, kagenta2a.AttachHITL(request, kagenta2a.ToolApprovalRequest{
		Type:  kagenta2a.HITLTypeToolApprovalRequest,
		Tools: []kagenta2a.HITLTool{{ID: "approval-1", Name: "delete_pod"}},
	}))
	response := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("Rejected"))
	require.NoError(t, kagenta2a.AttachHITL(response, kagenta2a.ToolApprovalResponse{
		Type:      kagenta2a.HITLTypeToolApprovalResponse,
		Approvals: []kagenta2a.ToolApproval{{ID: "approval-1"}},
	}))
	model.appendHistoryTask(historyTask("task-0", []*a2atype.Message{request, response}))

	model.appendUser("try again")
	model.submit("try again")
	streamEvent(model, a2atype.NewArtifactEvent(reqCtx(), dataPart("function_response", "delete_pod",
		map[string]any{"response": map[string]any{"error": `error tool "delete_pod" call is rejected`}})))

	assert.Contains(t, shownText(model), "⊘ delete_pod")
}

func selectKeys(m *chatModel, keys ...tea.KeyMsg) {
	for _, k := range keys {
		m.Update(k)
	}
}

func TestChatModelSelectCursorMoves(t *testing.T) {
	model := newTestChatModel()
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	for _, text := range []string{"a", "b", "c"} {
		model.appendUser(text)
	}
	model.Update(tea.KeyMsg{Type: tea.KeyCtrlG})

	tests := []struct {
		key  tea.KeyMsg
		want int
	}{
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")}, 0},
		{tea.KeyMsg{Type: tea.KeyUp}, 0},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}, 1},
		{tea.KeyMsg{Type: tea.KeyDown}, 2},
		{tea.KeyMsg{Type: tea.KeyDown}, 2},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")}, 1},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")}, 2},
	}
	for _, tt := range tests {
		model.Update(tt.key)
		assert.Equal(t, tt.want, model.selected, tt.key.String())
	}
}

// Fold indices are positions in the visible transcript, so growth does not disturb them.
func TestChatModelExpandedEntryStaysExpandedAsEntriesStreamIn(t *testing.T) {
	model := newTestChatModel()
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	model.appendUser("hi")
	model.appendEntry(transcript.ToolActivity{ID: "a", Name: "one", Outcome: transcript.Returned{Response: map[string]any{"result": "first-body"}}})
	model.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	require.Contains(t, model.vp.View(), "first-body")

	streamEvent(model, a2atype.NewArtifactEvent(reqCtx(), a2atype.NewTextPart("more")))
	streamEvent(model, a2atype.NewArtifactEvent(reqCtx(), dataPart("function_call", "two", map[string]any{})))

	assert.Contains(t, model.vp.View(), "first-body", "still expanded")
	assert.True(t, model.folds.Expanded(1))
	assert.False(t, model.folds.Expanded(3), "the new entry follows the default")
}

func TestChatModelSelectedEntryScrollsIntoView(t *testing.T) {
	model := newTestChatModel()
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 8}) // 5-line viewport
	for i := range 10 {
		model.appendUser(fmt.Sprintf("message-%d", i))
	}
	model.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	selectKeys(model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	assert.Contains(t, model.vp.View(), "message-0", "g scrolls to the top")

	selectKeys(model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	assert.Contains(t, model.vp.View(), "message-9")
	assert.NotContains(t, model.vp.View(), "message-0")
}

func TestChatModelSelectModeDoesNotJumpToNewestOnStream(t *testing.T) {
	model := newTestChatModel()
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 8})
	for i := range 10 {
		model.appendUser(fmt.Sprintf("message-%d", i))
	}
	model.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	selectKeys(model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})

	streamEvent(model, a2atype.NewArtifactEvent(reqCtx(), a2atype.NewTextPart("streamed")))

	assert.Contains(t, model.vp.View(), "message-0")
}

var esc = tea.KeyMsg{Type: tea.KeyEsc}

// streamingChat returns a chat whose turn is running; withTask feeds an event naming task-1.
func streamingChat(t *testing.T, withTask bool) (*chatModel, *fakeTurnClient) {
	t.Helper()
	client := &fakeTurnClient{}
	m := newChatModel(context.Background(), "reporter", "ctx-1", client, false)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	m.submit("hi")
	if withTask {
		deliver(m, clia2a.StreamResult{Event: a2atype.NewStatusUpdateEvent(reqCtx(), a2atype.TaskStateWorking, nil)})
	}
	require.True(t, m.isStreaming())
	return m, client
}

// runCmd runs a command and every command it batches, returning their messages.
// Callers must not pass a tea.Tick, which sleeps.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	var msgs []tea.Msg
	for _, c := range batch {
		msgs = append(msgs, runCmd(c)...)
	}
	return msgs
}

func armedAt(t *testing.T, m *chatModel) time.Time {
	t.Helper()
	turn, ok := m.turn.(*streamingTurn)
	require.True(t, ok, "the turn is streaming")
	return turn.cancelArmedAt
}

func TestChatModelSingleEscOnlyArmsCancel(t *testing.T) {
	m, client := streamingChat(t, true)

	_, cmd := m.Update(esc)

	assert.NotNil(t, cmd, "arming schedules the disarm tick")
	assert.Empty(t, client.cancels, "one esc does not cancel")
	assert.False(t, armedAt(t, m).IsZero())
	assert.Contains(t, m.View(), "press esc again to cancel")
	assert.True(t, m.isStreaming())
}

func TestChatModelDoubleEscCancelsTheTask(t *testing.T) {
	m, client := streamingChat(t, true)

	m.Update(esc)
	_, cmd := m.Update(esc)
	msgs := runCmd(cmd)

	require.Len(t, msgs, 1)
	assert.NotEqual(t, tea.Quit(), msgs[0], "esc never quits")
	assert.Equal(t, []a2atype.TaskID{"task-1"}, client.cancels)
	assert.NotContains(t, m.View(), "press esc again to cancel")

	m.Update(msgs[0])
	assert.True(t, m.isStreaming(), "the stream stays open until the canceled status arrives")
	assert.Contains(t, m.View(), "Canceling")

	m.Update(esc)
	_, cmd = m.Update(esc)
	assert.Empty(t, runCmd(cmd), "a cancel in flight is not sent twice")
	assert.Len(t, client.cancels, 1)

	deliver(m, clia2a.StreamResult{Event: a2atype.NewStatusUpdateEvent(reqCtx(), a2atype.TaskStateCanceled, nil)})
	m.Update(streamDoneMsg{gen: m.streamGen})
	assert.False(t, m.isStreaming())
	assert.Contains(t, shownText(m), "Task canceled.")
}

func TestChatModelCancelDisarms(t *testing.T) {
	tests := []struct {
		name   string
		disarm func(m *chatModel)
	}{
		{"after the timeout", func(m *chatModel) { m.Update(cancelDisarmMsg{armedAt: armedAt(t, m)}) }},
		{"on another key", func(m *chatModel) { m.Update(runes("a")) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, client := streamingChat(t, true)

			m.Update(esc)
			tt.disarm(m)

			assert.True(t, armedAt(t, m).IsZero())
			assert.NotContains(t, m.View(), "press esc again to cancel")
			m.Update(esc) // arms again rather than cancelling
			assert.Empty(t, client.cancels)
		})
	}
}

// A late tick from an earlier arm must not disarm a fresh one.
func TestChatModelStaleDisarmKeepsTheNewArm(t *testing.T) {
	m, _ := streamingChat(t, true)

	m.Update(esc)
	stale := armedAt(t, m)
	m.Update(runes("a"))
	m.Update(esc)
	m.Update(cancelDisarmMsg{armedAt: stale.Add(-time.Second)})

	assert.False(t, armedAt(t, m).IsZero())
}

func TestChatModelCancelBeforeTheFirstEventWaitsForATaskID(t *testing.T) {
	m, client := streamingChat(t, false)

	m.Update(esc)
	_, cmd := m.Update(esc)
	assert.Empty(t, runCmd(cmd))
	assert.Empty(t, client.cancels, "no task id yet")

	cmd = deliver(m, clia2a.StreamResult{Event: a2atype.NewStatusUpdateEvent(reqCtx(), a2atype.TaskStateWorking, nil)})
	runCmd(cmd)

	assert.Equal(t, []a2atype.TaskID{"task-1"}, client.cancels, "the first event with a task id fires the cancel")
}

func TestChatModelCancelErrorIsABannerAndCanRetry(t *testing.T) {
	m, client := streamingChat(t, true)
	client.cancelErr = errors.New("task is not cancelable")

	m.Update(esc)
	_, cmd := m.Update(esc)
	for _, msg := range runCmd(cmd) {
		m.Update(msg)
	}

	assert.Contains(t, shownText(m), "task is not cancelable")
	assert.True(t, m.isStreaming())

	m.Update(esc)
	_, cmd = m.Update(esc)
	runCmd(cmd)
	assert.Len(t, client.cancels, 2, "a failed cancel can be retried")
}

// In select mode esc means "back to the composer", even while a turn runs.
func TestChatModelEscInSelectModeOnlyLeavesSelectMode(t *testing.T) {
	m, client := streamingChat(t, true)
	m.appendUser("pick me")
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	require.Equal(t, modeSelect, m.mode)

	m.Update(esc)

	assert.Equal(t, modeCompose, m.mode)
	assert.True(t, armedAt(t, m).IsZero(), "leaving select mode does not arm")
	assert.Empty(t, client.cancels)
}

func TestChatModelEscWhileIdleDoesNothing(t *testing.T) {
	m := newTestChatModel()

	_, cmd := m.Update(esc)

	assert.Nil(t, cmd)
	assert.NotContains(t, m.View(), "press esc again to cancel")
}

// A message from a stream that already ended must not reopen or extend the turn.
func TestChatModelIgnoresStreamResultsWhileIdle(t *testing.T) {
	m := newTestChatModel()

	deliver(m, clia2a.StreamResult{Event: a2atype.NewArtifactEvent(reqCtx(), a2atype.NewTextPart("late"))})

	assert.Empty(t, m.entries)
}

func lastBanner(t *testing.T, m *chatModel) transcript.Banner {
	t.Helper()
	for i := len(m.entries) - 1; i >= 0; i-- {
		if banner, ok := m.entries[i].(transcript.Banner); ok {
			return banner
		}
	}
	require.Fail(t, "no banner shown")
	return transcript.Banner{}
}

func TestChatModelStateBannersNameTheStatePlainly(t *testing.T) {
	tests := []struct {
		name  string
		state a2atype.TaskState
		kind  transcript.BannerKind
		want  string
	}{
		{"canceled is information, since the user asked", a2atype.TaskStateCanceled, transcript.BannerInfo, "Task canceled."},
		{"failed is an error", a2atype.TaskStateFailed, transcript.BannerError, "Task failed."},
		{"rejected is an error", a2atype.TaskStateRejected, transcript.BannerError, "Task rejected."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestChatModel()
			streamEvent(m, a2atype.NewStatusUpdateEvent(reqCtx(), tt.state, nil))

			banner := lastBanner(t, m)
			assert.Equal(t, tt.kind, banner.Kind)
			assert.Contains(t, banner.Text, tt.want)
			assert.NotContains(t, banner.Text, "TASK_STATE")
		})
	}
}

func TestStateLabel(t *testing.T) {
	assert.Equal(t, "completed", stateLabel(a2atype.TaskStateCompleted))
	assert.Equal(t, "input required", stateLabel(a2atype.TaskStateInputRequired))
}

// A cancel that fails after its stream was replaced must not touch the new turn.
func TestChatModelStaleCancelFailureKeepsTheNewTurnsState(t *testing.T) {
	m, client := streamingChat(t, true)
	m.Update(esc)
	_, cmd := m.Update(esc)
	stale := runCmd(cmd)
	require.Len(t, stale, 1)
	oldGen := m.turn.(*streamingTurn).gen

	m.submit("again") // replaces the stream
	require.NotEqual(t, oldGen, m.turn.(*streamingTurn).gen)
	m.Update(esc)
	m.Update(esc) // requested, no task id yet
	require.Contains(t, m.View(), "Canceling")

	failure := stale[0].(cancelResultMsg)
	failure.err = errors.New("late failure")
	m.Update(failure)

	assert.Contains(t, m.View(), "Canceling", "the new turn's cancel is untouched")
	assert.Len(t, client.cancels, 1)
}

// A cancel that succeeds after the turn paused leaves no prompt for a dead task.
func TestChatModelLateCancelSuccessDropsThePromptOfTheCanceledTask(t *testing.T) {
	m, _ := streamingChat(t, true)
	m.Update(esc)
	_, cmd := m.Update(esc)
	msgs := runCmd(cmd)
	require.Len(t, msgs, 1)
	pause(m, approvalStatus(t, deletePodTool))
	_, awaiting := m.turn.(*awaitingTurn)
	require.True(t, awaiting)

	m.Update(msgs[0])

	_, idle := m.turn.(idleTurn)
	assert.True(t, idle, "the canceled task's request is gone")
	assert.Equal(t, transcript.BannerInfo, lastBanner(t, m).Kind)
	assert.Contains(t, lastBanner(t, m).Text, "canceled")
}

func TestChatModelCancelDroppedByAPauseExplainsItself(t *testing.T) {
	m, client := streamingChat(t, false)
	m.Update(esc)
	m.Update(esc)

	pause(m, approvalStatus(t, deletePodTool))

	assert.Empty(t, client.cancels)
	banner := lastBanner(t, m)
	assert.Equal(t, transcript.BannerInfo, banner.Kind)
	assert.Contains(t, banner.Text, "waiting for input")
	assert.Contains(t, banner.Text, "ctrl+x")
}

func TestChatModelIgnoresSendWhileHistoryLoads(t *testing.T) {
	client := &fakeTurnClient{}
	m := newChatModel(context.Background(), "reporter", "ctx-1", client, false)
	m.historyPending = true
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	m.input.SetValue("hello")

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	assert.Empty(t, client.sent)
	assert.Equal(t, "hello", m.input.Value(), "the draft is kept")
	assert.False(t, m.isStreaming())

	m.historyPending = false
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Len(t, client.sent, 1)
}
