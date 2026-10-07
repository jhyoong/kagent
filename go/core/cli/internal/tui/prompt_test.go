package tui

import (
	"errors"
	"strings"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	tea "github.com/charmbracelet/bubbletea"
	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
	clia2a "github.com/kagent-dev/kagent/go/core/cli/internal/a2a"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/transcript"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	deletePodTool = kagenta2a.HITLTool{ID: "approval-1", CallID: "call-1", Name: "k8s_delete_pod", Args: map[string]any{"pod": "checkout-7d9f", "namespace": "shop"}}
	scaleTool     = kagenta2a.HITLTool{ID: "approval-2", CallID: "call-2", Name: "k8s_scale", Args: map[string]any{"deployment": "checkout", "replicas": float64(0)}}
)

const pauseProse = "Deleting this pod restarts checkout (k8s_delete_pod, k8s_scale)"

// approvalStatus is the input-required status message of a tool approval for tools.
func approvalStatus(t *testing.T, tools ...kagenta2a.HITLTool) *a2atype.Message {
	t.Helper()
	message := a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart(pauseProse))
	require.NoError(t, kagenta2a.AttachHITL(message, kagenta2a.ToolApprovalRequest{
		Type: kagenta2a.HITLTypeToolApprovalRequest, Hint: pauseProse, Tools: tools,
	}))
	return message
}

// pause streams the input-required event that parks the running turn with message.
func pause(m *chatModel, message *a2atype.Message) {
	deliver(m, clia2a.StreamResult{Event: a2atype.NewStatusUpdateEvent(reqCtx(), a2atype.TaskStateInputRequired, message)})
}

// pausedChat returns a chat whose turn parked asking to approve tools.
func pausedChat(t *testing.T, tools ...kagenta2a.HITLTool) (*chatModel, *fakeTurnClient) {
	t.Helper()
	m, client := streamingChat(t, true)
	pause(m, approvalStatus(t, tools...))
	_, awaiting := m.turn.(*awaitingTurn)
	require.True(t, awaiting, "the turn waits for an answer")
	return m, client
}

func press(m *chatModel, keys ...string) tea.Cmd {
	var cmds []tea.Cmd
	for _, key := range keys {
		var msg tea.KeyMsg
		switch key {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "space":
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
		case "ctrl+x":
			msg = tea.KeyMsg{Type: tea.KeyCtrlX}
		case "ctrl+p":
			msg = tea.KeyMsg{Type: tea.KeyCtrlP}
		default:
			msg = runes(key)
		}
		_, cmd := m.Update(msg)
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

func TestChatModelApprovalResumesThePausedTask(t *testing.T) {
	m, client := pausedChat(t, deletePodTool, scaleTool)

	view := m.View()
	assert.Contains(t, view, "reporter is asking permission to run 2 tools")
	assert.Equal(t, 1, strings.Count(view, pauseProse), "the status prose shows once")
	assert.Contains(t, view, `namespace="shop", pod="checkout-7d9f"`, "an argument preview per tool")
	assert.Contains(t, view, "enter send (0/2)")
	assert.NotContains(t, shownText(m), "Input required", "the false resume banner is gone")

	press(m, "a")
	press(m, "enter")
	require.Len(t, client.sent, 1, "enter waits until every tool is decided")
	assert.Contains(t, m.View(), "enter send (1/2)")

	press(m, "a", "enter")

	require.Len(t, client.sent, 2)
	sent := client.sent[1].Message
	assert.Equal(t, a2atype.TaskID("task-1"), sent.TaskID, "the answer resumes the paused task")
	assert.Equal(t, "ctx-1", sent.ContextID)
	assert.Contains(t, sent.Extensions, kagenta2a.HITLExtensionURI)
	response, err := kagenta2a.ParseToolApprovalResponse(sent)
	require.NoError(t, err)
	assert.Equal(t, []kagenta2a.ToolApproval{{ID: "approval-1", Approved: true}, {ID: "approval-2", Approved: true}}, response.Approvals)

	assert.True(t, m.isStreaming())
	assert.Contains(t, shownText(m), "✓ Approved k8s_delete_pod · ✓ Approved k8s_scale")
	assert.NotContains(t, shownText(m), "You: Approved", "the record is shown, not the fallback text")
	assert.Contains(t, m.View(), "> ", "the composer is back")
}

func TestChatModelRejectTakesAReason(t *testing.T) {
	m, client := pausedChat(t, deletePodTool, scaleTool)

	press(m, "a", "r")
	assert.Contains(t, m.View(), "reason ›")
	assert.Contains(t, m.View(), "enter confirm · esc back")
	press(m, "t", "o", "o", " ", "r", "i", "s", "k", "y", "enter")

	assert.Contains(t, m.View(), "reason › too risky")
	press(m, "enter")
	require.Len(t, client.sent, 2)
	response, err := kagenta2a.ParseToolApprovalResponse(client.sent[1].Message)
	require.NoError(t, err)
	assert.Equal(t, []kagenta2a.ToolApproval{
		{ID: "approval-1", Approved: true},
		{ID: "approval-2", RejectionReason: "too risky"},
	}, response.Approvals)
	assert.Contains(t, shownText(m), `✗ Rejected k8s_scale: "too risky"`)
}

func TestChatModelEscLeavesTheReasonUndecided(t *testing.T) {
	m, client := pausedChat(t, deletePodTool)

	press(m, "r", "n", "o", "esc", "enter")

	assert.Len(t, client.sent, 1, "esc backs out of the rejection")
	assert.Contains(t, m.View(), "[ ] k8s_delete_pod")
}

func TestChatModelApproveAllAndArgs(t *testing.T) {
	m, client := pausedChat(t, deletePodTool, scaleTool)

	press(m, "down", "space")
	assert.Contains(t, m.View(), `"deployment": "checkout"`, "space shows the full arguments")
	press(m, "space")
	assert.NotContains(t, m.View(), `"deployment": "checkout"`)

	press(m, "A", "enter")
	require.Len(t, client.sent, 2)
}

// The prompt is part of the chat's height, so the transcript gives up exactly the lines it takes.
func TestChatModelPromptTakesItsMeasuredHeight(t *testing.T) {
	m, _ := streamingChat(t, true)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 20}) // wide enough that no prompt line wraps
	composing := m.vp.Height

	pause(m, approvalStatus(t, deletePodTool, scaleTool))

	promptLines := strings.Count(m.bottomView(m.vp.Width), "\n") + 1
	assert.Equal(t, 5, promptLines, "heading, hint, two tools and the hint line")
	assert.Equal(t, composing-(promptLines-1), m.vp.Height)
	assert.Equal(t, 20, strings.Count(m.View(), "\n")+1, "the chat still fills its height")
}

func TestChatModelDiscard(t *testing.T) {
	tests := []struct {
		name         string
		cancelErr    error
		cancelResult *a2atype.Task
		wantAwaiting bool
		want         string
	}{
		{name: "a canceled task returns to the composer", want: "Discarded the request"},
		{name: "a failed cancel keeps the prompt", cancelErr: errors.New("unavailable"), wantAwaiting: true, want: "Discard failed: unavailable"},
		{
			name:         "a task still waiting shows its request again",
			cancelResult: &a2atype.Task{ID: "task-1", ContextID: "ctx-1", Status: a2atype.TaskStatus{State: a2atype.TaskStateInputRequired}},
			wantAwaiting: true, want: "still waiting",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, client := pausedChat(t, deletePodTool)
			client.cancelErr, client.cancelResult = tt.cancelErr, tt.cancelResult

			press(m, "ctrl+x")
			assert.Contains(t, m.View(), "y discard")
			press(m, "a")
			assert.Equal(t, hitlVerdictOf(m), "[ ]", "keys other than y, n and esc do nothing while confirming")
			press(m, "n")
			assert.NotContains(t, m.View(), "y discard", "n keeps the request")
			assert.Empty(t, client.cancels)

			press(m, "ctrl+x")
			msgs := runCmd(press(m, "y"))
			require.Len(t, msgs, 1)
			assert.Equal(t, []a2atype.TaskID{"task-1"}, client.cancels)
			m.Update(msgs[0])

			_, awaiting := m.turn.(*awaitingTurn)
			assert.Equal(t, tt.wantAwaiting, awaiting)
			assert.Contains(t, shownText(m), tt.want)
			if !awaiting {
				assert.Contains(t, m.View(), "Type a message", "the composer is back")
			}
		})
	}
}

// hitlVerdictOf is the first tool row's verdict mark in the prompt.
func hitlVerdictOf(m *chatModel) string {
	for _, line := range strings.Split(m.bottomView(m.vp.Width), "\n") {
		if at := strings.Index(line, "["); at >= 0 && strings.Contains(line, "k8s_delete_pod") {
			return line[at : at+3]
		}
	}
	return ""
}

func TestChatModelUnknownRequestOffersOnlyDiscard(t *testing.T) {
	tests := []struct {
		name    string
		message *a2atype.Message
		want    string
	}{
		{
			name:    "prose only",
			message: a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("Human input is required before the agent can continue.")),
			want:    `"Human input is required before the agent can continue."`,
		},
		{
			name: "nested ask_user, whose answer id is unsettled",
			message: func() *a2atype.Message {
				message := a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("Which namespace?"))
				require.NoError(t, kagenta2a.AttachHITL(message, kagenta2a.AskUserRequest{
					Type: kagenta2a.HITLTypeAskUserRequest, ID: "ask-1",
					Questions: []kagenta2a.HITLQuestion{{Question: "Which namespace?"}},
					Nested:    &kagenta2a.NestedHITLRequest{SubagentName: "billing-agent", TaskID: "child", Tools: []kagenta2a.HITLTool{{ID: "child-ask", Name: "ask_user"}}},
				}))
				return message
			}(),
			want: "Which namespace?",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, client := streamingChat(t, true)
			pause(m, tt.message)

			assert.Contains(t, m.View(), tt.want)
			assert.Contains(t, m.View(), "ctrl+x discard the request")
			press(m, "a", "A", "enter")
			assert.Len(t, client.sent, 1, "nothing to answer")

			press(m, "ctrl+x")
			runCmd(press(m, "y"))
			assert.Equal(t, []a2atype.TaskID{"task-1"}, client.cancels)
		})
	}
}

// The paused task's output is already shown; a resumed stream that repeats it must not show it twice.
func TestChatModelResumeDoesNotRepeatPausedText(t *testing.T) {
	before := &a2atype.Artifact{ID: "before", Parts: a2atype.ContentParts{a2atype.NewTextPart("Checking the pod.")}}
	after := &a2atype.Artifact{ID: "after", Parts: a2atype.ContentParts{a2atype.NewTextPart("Pod deleted.")}}
	tests := []struct {
		name  string
		first a2atype.Event
	}{
		{
			name: "a snapshot of the whole task",
			first: &a2atype.Task{
				ID: "task-1", ContextID: "ctx-1",
				Status:    a2atype.TaskStatus{State: a2atype.TaskStateWorking},
				Artifacts: []*a2atype.Artifact{before, after},
			},
		},
		{
			name:  "an incremental artifact",
			first: &a2atype.TaskArtifactUpdateEvent{TaskID: "task-1", ContextID: "ctx-1", Artifact: after},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := streamingChat(t, false)
			deliver(m, clia2a.StreamResult{Event: &a2atype.TaskArtifactUpdateEvent{TaskID: "task-1", ContextID: "ctx-1", Artifact: before}})
			pause(m, approvalStatus(t, deletePodTool))
			press(m, "a", "enter")

			deliver(m, clia2a.StreamResult{Event: tt.first})

			got := shownText(m)
			assert.Equal(t, 1, strings.Count(got, "Checking the pod."), got)
			assert.Contains(t, got, "Agent:\nPod deleted.")
		})
	}
}

// Resume starts a stream right after the paused one; anything the paused stream still delivers is stale.
func TestChatModelStaleStreamMessagesDoNotTouchTheResumedTurn(t *testing.T) {
	m, _ := pausedChat(t, deletePodTool)
	stale := m.streamGen
	press(m, "a", "enter")
	require.True(t, m.isStreaming())

	m.Update(streamDoneMsg{gen: stale})
	m.Update(streamMsg{gen: stale, result: clia2a.StreamResult{Err: errors.New("stream closed")}})
	m.Update(streamMsg{gen: stale, result: clia2a.StreamResult{Event: a2atype.NewArtifactEvent(reqCtx(), a2atype.NewTextPart("late"))}})

	assert.True(t, m.isStreaming(), "the resumed turn keeps running")
	assert.NotContains(t, shownText(m), "Connection error")
	assert.NotContains(t, shownText(m), "late")

	m.Update(streamDoneMsg{gen: m.streamGen})
	assert.False(t, m.isStreaming(), "its own end still ends it")
}

// A rejection record hides the refused call's not-run entry once the resumed
// stream settles it; folds must stay with their entries, not their positions.
func TestChatModelFoldsFollowEntriesWhenARecordHidesOne(t *testing.T) {
	m, _ := streamingChat(t, true)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	deliver(m, clia2a.StreamResult{Event: a2atype.NewArtifactEvent(reqCtx(), dataPart("function_call", "k8s_delete_pod", map[string]any{}))})
	deliver(m, clia2a.StreamResult{Event: a2atype.NewArtifactEvent(reqCtx(), dataPart("function_response", "k8s_delete_pod",
		map[string]any{"response": map[string]any{"error": `error tool "k8s_delete_pod" requires confirmation, please approve or reject`}}))})
	m.appendEntry(transcript.ToolActivity{ID: "call-9", Name: "get_logs", Outcome: transcript.Returned{Response: map[string]any{"result": "logs-body"}}})
	pause(m, approvalStatus(t, deletePodTool))

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // expand get_logs, the newest entry
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	require.Contains(t, m.vp.View(), "logs-body")
	logs := len(m.visibleEntries()) - 1

	press(m, "r", "enter", "enter")
	deliver(m, clia2a.StreamResult{Event: a2atype.NewArtifactEvent(reqCtx(), dataPart("function_response", "k8s_delete_pod",
		map[string]any{"response": map[string]any{"error": `error tool "k8s_delete_pod" call is rejected`}}))})

	visible := m.visibleEntries()
	require.NotContains(t, shownText(m), "⊘ k8s_delete_pod", "the record explains the refusal")
	assert.Equal(t, transcript.ToolActivity{ID: "call-9", Name: "get_logs", Outcome: transcript.Returned{Response: map[string]any{"result": "logs-body"}}}, visible[logs-1])
	assert.True(t, m.folds.Expanded(logs-1), "the fold moved with get_logs")
	assert.False(t, m.folds.Expanded(logs), "the record did not inherit it")
	assert.Contains(t, m.vp.View(), "logs-body")
}

func TestChatModelRestorePendingShowsThePrompt(t *testing.T) {
	m := newTestChatModel()
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	paused := &a2atype.Task{
		ID: "task-1", ContextID: "ctx-1",
		Status:    a2atype.TaskStatus{State: a2atype.TaskStateInputRequired, Message: approvalStatus(t, deletePodTool)},
		History:   []*a2atype.Message{a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("delete the pod"))},
		Artifacts: []*a2atype.Artifact{{ID: "before", Parts: a2atype.ContentParts{a2atype.NewTextPart("Checking the pod.")}}},
	}

	m.restorePending(paused)

	assert.Contains(t, shownText(m), "delete the pod")
	assert.Contains(t, m.View(), "asking permission to run 1 tool")
	client := m.client.(*fakeTurnClient)
	press(m, "a", "enter")
	require.Len(t, client.sent, 1)
	assert.Equal(t, a2atype.TaskID("task-1"), client.sent[0].Message.TaskID)

	deliver(m, clia2a.StreamResult{Event: &a2atype.TaskArtifactUpdateEvent{
		TaskID: "task-1", ContextID: "ctx-1",
		Artifact: &a2atype.Artifact{ID: "after", Parts: a2atype.ContentParts{a2atype.NewTextPart("Pod deleted.")}},
	}})
	assert.Equal(t, 1, strings.Count(shownText(m), "Checking the pod."))
}

func TestChatModelRestorePendingWaitsForTheRunningTurn(t *testing.T) {
	m, _ := streamingChat(t, true)

	m.restorePending(&a2atype.Task{ID: "task-0", Status: a2atype.TaskStatus{State: a2atype.TaskStateInputRequired}})

	assert.True(t, m.isStreaming(), "the turn the user started wins")
}
