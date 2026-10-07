package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	tea "github.com/charmbracelet/bubbletea"
	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
	clia2a "github.com/kagent-dev/kagent/go/core/cli/internal/a2a"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/transcript"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestChatModel() *chatModel {
	send := func(context.Context, *a2atype.SendMessageRequest) <-chan clia2a.StreamResult {
		ch := make(chan clia2a.StreamResult)
		close(ch)
		return ch
	}
	return newChatModel(context.Background(), "reporter", "ctx-1", send, false)
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
				model.appendEvent(event)
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
	}{
		{
			name: "input required is paused",
			apply: func(m *chatModel) {
				m.appendEvent(a2atype.NewStatusUpdateEvent(reqCtx(), a2atype.TaskStateInputRequired, nil))
			},
			want: "Input required", wantAbsent: "✗",
		},
		{
			name: "auth required is paused",
			apply: func(m *chatModel) {
				m.appendEvent(a2atype.NewStatusUpdateEvent(reqCtx(), a2atype.TaskStateAuthRequired, nil))
			},
			want: "Authentication required", wantAbsent: "✗",
		},
		{
			name: "failed is an error and keeps its explanation",
			apply: func(m *chatModel) {
				message := a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("execution failed"))
				m.appendEvent(a2atype.NewStatusUpdateEvent(reqCtx(), a2atype.TaskStateFailed, message))
			},
			want: "execution failed",
		},
		{
			name: "completed needs no banner",
			apply: func(m *chatModel) {
				m.appendEvent(a2atype.NewStatusUpdateEvent(reqCtx(), a2atype.TaskStateCompleted, nil))
			},
			wantAbsent: "✗",
		},
		{
			name: "a transport failure is not a task failure",
			apply: func(m *chatModel) {
				m.Update(clia2a.StreamResult{Err: errors.New("stream disconnected")})
			},
			want: "Connection error: stream disconnected", wantAbsent: "✗ Task",
		},
		{
			// A malformed stream is neither a task nor a transport failure, and must not be dropped.
			name: "a malformed stream is a protocol error",
			apply: func(m *chatModel) {
				m.appendEvent(a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("first")))
				m.appendEvent(a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("second")))
			},
			want: "Protocol error",
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
			assert.False(t, model.working, "a settled task stops the working indicator")
		})
	}
}

func TestChatModelRendersToolActivityBeforeLastChunk(t *testing.T) {
	model := newTestChatModel()
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 20})

	model.appendEvent(a2atype.NewArtifactEvent(reqCtx(), a2atype.NewTextPart("checking")))
	model.appendEvent(a2atype.NewArtifactEvent(reqCtx(),
		dataPart("function_call", "get_pods", map[string]any{"args": map[string]any{"namespace": "default"}})))
	model.appendEvent(a2atype.NewArtifactEvent(reqCtx(),
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
	model.appendEvent(first)
	model.appendEvent(a2atype.NewArtifactEvent(reqCtx(), dataPart("function_call", "get_pods", map[string]any{})))
	model.appendEvent(a2atype.NewArtifactUpdateEvent(reqCtx(), first.Artifact.ID, a2atype.NewTextPart(" after")))

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
	model.appendEvent(a2atype.NewArtifactEvent(reqCtx(), a2atype.NewTextPart("checking")))
	model.appendEvent(a2atype.NewArtifactEvent(reqCtx(), dataPart("function_call", "get_pods", map[string]any{})))
	model.appendEvent(a2atype.NewArtifactEvent(reqCtx(), dataPart("function_response", "get_pods", map[string]any{"response": "new"})))

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
	model.appendEvent(a2atype.NewArtifactEvent(reqCtx(), dataPart("function_response", "delete_pod",
		map[string]any{"response": map[string]any{"error": `error tool "delete_pod" call is rejected`}})))

	assert.Contains(t, shownText(model), "⊘ delete_pod")
}
