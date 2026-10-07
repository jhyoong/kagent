package transcript

import (
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	confirmationError = `error tool "delete_pod" requires confirmation, please approve or reject`
	rejectionError    = `error tool "delete_pod" call is rejected`
)

func userText(id, text string) *a2atype.Message {
	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart(text))
	message.ID = id
	return message
}

func agentText(id, text string) *a2atype.Message {
	message := a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart(text))
	message.ID = id
	return message
}

func withHITL(t *testing.T, message *a2atype.Message, payload any) *a2atype.Message {
	t.Helper()
	require.NoError(t, kagenta2a.AttachHITL(message, payload))
	return message
}

func artifact(id string, parts ...*a2atype.Part) *a2atype.Artifact {
	return &a2atype.Artifact{ID: a2atype.ArtifactID(id), Parts: parts}
}

func positioned[T a2atype.MetadataCarrier](carrier T, offset int) T {
	kagenta2a.SetTimelinePosition(carrier, time.Date(2026, 1, 1, 0, 0, offset, 0, time.UTC))
	return carrier
}

func completed(history []*a2atype.Message, artifacts ...*a2atype.Artifact) *a2atype.Task {
	return &a2atype.Task{
		ID: "task-1", ContextID: "ctx-1",
		Status:    a2atype.TaskStatus{State: a2atype.TaskStateCompleted},
		History:   history,
		Artifacts: artifacts,
	}
}

func TestProjectTask(t *testing.T) {
	tests := []struct {
		name string
		task func(t *testing.T) *a2atype.Task
		want []Entry
	}{
		{
			name: "nil task projects nothing",
			task: func(*testing.T) *a2atype.Task { return nil },
			want: nil,
		},
		{
			name: "user turn then artifact output",
			task: func(*testing.T) *a2atype.Task {
				return completed(
					[]*a2atype.Message{userText("u1", "what is 2+2?")},
					artifact("a1", a2atype.NewTextPart("The answer is 4.")),
				)
			},
			want: []Entry{UserMessage{Text: "what is 2+2?"}, AgentText{Text: "The answer is 4."}},
		},
		{
			name: "a repeated history message is shown once",
			task: func(*testing.T) *a2atype.Task {
				return completed([]*a2atype.Message{userText("u1", "hi"), userText("u1", "hi")})
			},
			want: []Entry{UserMessage{Text: "hi"}},
		},
		{
			name: "an artifact repeating history text is the same reply",
			task: func(*testing.T) *a2atype.Task {
				return completed(
					[]*a2atype.Message{userText("u1", "hi"), agentText("m1", "hello")},
					artifact("a1", a2atype.NewTextPart("hello")),
				)
			},
			want: []Entry{UserMessage{Text: "hi"}, AgentText{Text: "hello"}},
		},
		{
			name: "a call and its result pair by id into one entry",
			task: func(*testing.T) *a2atype.Task {
				return completed(
					[]*a2atype.Message{userText("u1", "pods?")},
					artifact("a1", kagenta2a.NewToolCallPart("c1", "get_pods", map[string]any{"namespace": "shop"})),
					artifact("a2", a2atype.NewTextPart("checking")),
					artifact("a3", kagenta2a.NewToolResultPart("c1", "get_pods", map[string]any{"pods": []any{"a"}})),
				)
			},
			want: []Entry{
				UserMessage{Text: "pods?"},
				ToolActivity{ID: "c1", Name: "get_pods", Args: map[string]any{"namespace": "shop"},
					Outcome: Returned{Response: map[string]any{"pods": []any{"a"}}}},
				AgentText{Text: "checking"},
			},
		},
		{
			name: "a call with no result is still running",
			task: func(*testing.T) *a2atype.Task {
				return completed(nil, artifact("a1", kagenta2a.NewToolCallPart("c1", "get_pods", nil)))
			},
			want: []Entry{ToolActivity{ID: "c1", Name: "get_pods", Outcome: Running{}}},
		},
		{
			name: "an approval exchange is one record and its rejected result is hidden",
			task: func(t *testing.T) *a2atype.Task {
				request := withHITL(t, agentText("req", "Tool request approval"), kagenta2a.ToolApprovalRequest{
					Type:  kagenta2a.HITLTypeToolApprovalRequest,
					Tools: []kagenta2a.HITLTool{{ID: "approval-1", Name: "delete_pod", Args: map[string]any{}}},
				})
				response := withHITL(t, userText("resp", "Rejected: delete_pod"), kagenta2a.ToolApprovalResponse{
					Type:      kagenta2a.HITLTypeToolApprovalResponse,
					Approvals: []kagenta2a.ToolApproval{{ID: "approval-1", RejectionReason: "serving traffic"}},
				})
				return completed(
					[]*a2atype.Message{request, response},
					artifact("a1", kagenta2a.NewToolCallPart("c1", "delete_pod", map[string]any{"pod": "p"})),
					artifact("a2", kagenta2a.NewToolResultPart("c1", "delete_pod", map[string]any{"error": confirmationError})),
					artifact("a3", kagenta2a.NewToolResultPart("c1", "delete_pod", map[string]any{"error": rejectionError})),
				)
			},
			want: []Entry{ApprovalRecord{
				Tools:     []kagenta2a.HITLTool{{ID: "approval-1", Name: "delete_pod", Args: map[string]any{}}},
				Decisions: []kagenta2a.ToolApproval{{ID: "approval-1", RejectionReason: "serving traffic"}},
			}},
		},
		{
			name: "a nested approval records the child's tools and who asked",
			task: func(t *testing.T) *a2atype.Task {
				request := withHITL(t, agentText("req", "approve?"), kagenta2a.ToolApprovalRequest{
					Type:  kagenta2a.HITLTypeToolApprovalRequest,
					Tools: []kagenta2a.HITLTool{{ID: "parent", Name: "billing"}},
					Nested: &kagenta2a.NestedHITLRequest{
						SubagentName: "billing-agent",
						Tools:        []kagenta2a.HITLTool{{ID: "child", Name: "refund"}},
					},
				})
				response := withHITL(t, userText("resp", "Approved"), kagenta2a.ToolApprovalResponse{
					Type:      kagenta2a.HITLTypeToolApprovalResponse,
					Approvals: []kagenta2a.ToolApproval{{ID: "child", Approved: true}},
				})
				return completed([]*a2atype.Message{request, response})
			},
			want: []Entry{ApprovalRecord{
				Tools:     []kagenta2a.HITLTool{{ID: "child", Name: "refund"}},
				Decisions: []kagenta2a.ToolApproval{{ID: "child", Approved: true}},
				AskedBy:   "billing-agent",
			}},
		},
		{
			name: "an unpaired rejection shows as not run",
			task: func(*testing.T) *a2atype.Task {
				return completed(nil, artifact("a1", kagenta2a.NewToolResultPart("", "delete_pod", map[string]any{"error": rejectionError})))
			},
			want: []Entry{ToolActivity{Name: "delete_pod", Outcome: NotRun{}}},
		},
		{
			name: "an ask_user exchange is one record placed before the reply that follows it",
			task: func(t *testing.T) *a2atype.Task {
				request := withHITL(t, agentText("req", "What size?"), kagenta2a.AskUserRequest{
					Type: kagenta2a.HITLTypeAskUserRequest, ID: "q1",
					Questions: []kagenta2a.HITLQuestion{{Question: "What size?", Choices: []string{"Small", "Large"}}},
				})
				response := withHITL(t, userText("resp", "Large"), kagenta2a.AskUserResponse{
					Type: kagenta2a.HITLTypeAskUserResponse, ID: "q1",
					Answers: []kagenta2a.AskUserAnswer{{Answer: []string{"Large"}}},
				})
				return completed(
					[]*a2atype.Message{userText("u1", "order a shirt"), request, response},
					artifact("a1", kagenta2a.NewToolCallPart("ask", "ask_user", map[string]any{})),
					artifact("a2", kagenta2a.NewToolResultPart("ask", "ask_user", map[string]any{"result": "Large"})),
					artifact("a3", a2atype.NewTextPart("Ordered a large shirt.")),
				)
			},
			want: []Entry{
				UserMessage{Text: "order a shirt"},
				AnswerRecord{
					Questions: []kagenta2a.HITLQuestion{{Question: "What size?", Choices: []string{"Small", "Large"}}},
					Answers:   [][]string{{"Large"}},
				},
				AgentText{Text: "Ordered a large shirt."},
			},
		},
		{
			name: "timeline positions order history and artifacts together",
			task: func(*testing.T) *a2atype.Task {
				return completed(
					[]*a2atype.Message{positioned(userText("u0", "start"), 1), positioned(userText("u1", "answer"), 4)},
					positioned(artifact("a0", kagenta2a.NewToolCallPart("ask", "ask_user", nil)), 2),
					positioned(artifact("a1", a2atype.NewTextPart("Which topic?")), 3),
					positioned(artifact("a2", a2atype.NewTextPart("Thanks")), 5),
				)
			},
			want: []Entry{
				UserMessage{Text: "start"},
				AgentText{Text: "Which topic?"},
				UserMessage{Text: "answer"},
				AgentText{Text: "Thanks"},
			},
		},
		{
			name: "a partly positioned timeline still drops an artifact repeating history",
			task: func(*testing.T) *a2atype.Task {
				return completed(
					[]*a2atype.Message{positioned(userText("u0", "hi"), 1), positioned(agentText("m1", "Hello"), 2)},
					positioned(artifact("a1", a2atype.NewTextPart("Hello")), 3),
					artifact("a2", a2atype.NewTextPart("Bye")),
				)
			},
			want: []Entry{
				UserMessage{Text: "hi"},
				AgentText{Text: "Hello"},
				AgentText{Text: "Bye"},
			},
		},
		{
			name: "text on both sides of a tool call keeps its order within one message",
			task: func(*testing.T) *a2atype.Task {
				return completed(nil, artifact("a1",
					a2atype.NewTextPart("before "), a2atype.NewTextPart("call"),
					kagenta2a.NewToolCallPart("c1", "get_pods", nil),
					a2atype.NewTextPart("after"),
				))
			},
			want: []Entry{
				AgentText{Text: "before call"},
				ToolActivity{ID: "c1", Name: "get_pods", Outcome: Running{}},
				AgentText{Text: "after"},
			},
		},
		{
			name: "a confirmation request result is awaiting approval",
			task: func(*testing.T) *a2atype.Task {
				return completed(nil,
					artifact("a1", kagenta2a.NewToolCallPart("c1", "delete_pod", nil)),
					artifact("a2", kagenta2a.NewToolResultPart("c1", "delete_pod", map[string]any{"error": confirmationError})),
				)
			},
			want: []Entry{ToolActivity{ID: "c1", Name: "delete_pod", Outcome: AwaitingApproval{}}},
		},
		{
			name: "a malformed HITL payload is reported, not dropped",
			task: func(t *testing.T) *a2atype.Task {
				response := withHITL(t, userText("resp", "Approved"), map[string]any{
					"type": kagenta2a.HITLTypeToolApprovalResponse, "approvals": []any{},
				})
				return completed([]*a2atype.Message{response})
			},
			want: []Entry{
				Banner{Kind: BannerError, Text: "Malformed tool approval response: at least one tool approval decision is required"},
				UserMessage{Text: "Approved"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ProjectTask(tt.task(t)))
		})
	}
}

func TestClassifyResult(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		response any
		want     ToolOutcome
	}{
		{name: "a plain result returned", tool: "get_pods", response: map[string]any{"pods": 1}, want: Returned{Response: map[string]any{"pods": 1}}},
		{name: "a scalar result returned", tool: "echo", response: "hi", want: Returned{Response: "hi"}},
		{name: "confirmation required", tool: "delete_pod", response: map[string]any{"error": confirmationError}, want: AwaitingApproval{}},
		{name: "rejected", tool: "delete_pod", response: map[string]any{"error": rejectionError}, want: NotRun{}},
		{name: "a nameless result trusts the string", tool: "", response: map[string]any{"error": rejectionError}, want: NotRun{}},
		{
			name: "a control string naming another tool is a real error", tool: "get_pods",
			response: map[string]any{"error": rejectionError},
			want:     Returned{Response: map[string]any{"error": rejectionError}, Failed: true},
		},
		{
			name: "an error discussing approval is not control flow", tool: "delete_pod",
			response: map[string]any{"error": "the approval service " + rejectionError},
			want:     Returned{Response: map[string]any{"error": "the approval service " + rejectionError}, Failed: true},
		},
		{name: "isError fails", tool: "mcp", response: map[string]any{"isError": true}, want: Returned{Response: map[string]any{"isError": true}, Failed: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ClassifyResult(tt.tool, tt.response))
		})
	}
}

func TestVisible(t *testing.T) {
	rejected := ApprovalRecord{
		Tools:     []kagenta2a.HITLTool{{ID: "a1", Name: "delete_pod"}, {ID: "a2", Name: "get_logs"}},
		Decisions: []kagenta2a.ToolApproval{{ID: "a1"}, {ID: "a2", Approved: true}},
	}
	tests := []struct {
		name    string
		entries []Entry
		want    []Entry
	}{
		{
			name:    "ask_user activity is hidden",
			entries: []Entry{ToolActivity{Name: "ask_user", Outcome: Running{}}, AgentText{Text: "hi"}},
			want:    []Entry{AgentText{Text: "hi"}},
		},
		{
			name: "a not-run result covered by a rejection is hidden",
			entries: []Entry{
				ToolActivity{ID: "c1", Name: "delete_pod", Outcome: NotRun{}},
				ToolActivity{ID: "c2", Name: "get_logs", Outcome: Returned{}},
				rejected,
			},
			want: []Entry{ToolActivity{ID: "c2", Name: "get_logs", Outcome: Returned{}}, rejected},
		},
		{
			name: "a rejection with a call ID covers that call only, not others of the same name",
			entries: []Entry{
				ToolActivity{ID: "call-old", Name: "delete_pod", Outcome: NotRun{}},
				ToolActivity{ID: "call-new", Name: "delete_pod", Outcome: NotRun{}},
				ApprovalRecord{
					Tools:     []kagenta2a.HITLTool{{ID: "approval-1", CallID: "call-new", Name: "delete_pod"}},
					Decisions: []kagenta2a.ToolApproval{{ID: "approval-1"}},
				},
			},
			want: []Entry{
				ToolActivity{ID: "call-old", Name: "delete_pod", Outcome: NotRun{}},
				ApprovalRecord{
					Tools:     []kagenta2a.HITLTool{{ID: "approval-1", CallID: "call-new", Name: "delete_pod"}},
					Decisions: []kagenta2a.ToolApproval{{ID: "approval-1"}},
				},
			},
		},
		{
			name: "a rejection whose request is not in view covers nothing",
			entries: []Entry{
				ToolActivity{Name: "delete_pod", Outcome: NotRun{}},
				ApprovalRecord{Decisions: []kagenta2a.ToolApproval{{ID: "approval-1"}}},
			},
			want: []Entry{
				ToolActivity{Name: "delete_pod", Outcome: NotRun{}},
				ApprovalRecord{Decisions: []kagenta2a.ToolApproval{{ID: "approval-1"}}},
			},
		},
		{
			name:    "an uncovered not-run result stays",
			entries: []Entry{ToolActivity{Name: "drop_db", Outcome: NotRun{}}, rejected},
			want:    []Entry{ToolActivity{Name: "drop_db", Outcome: NotRun{}}, rejected},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Visible(tt.entries))
		})
	}
}

func TestApplyToolActivityUpsertsByCallID(t *testing.T) {
	entries := []Entry{AgentText{Text: "looking"}}
	entries = ApplyToolActivity(entries, kagenta2a.ToolActivity{Kind: kagenta2a.ToolCallKind, ID: "c1", Name: "get_pods", Args: map[string]any{"n": 1}})
	entries = append(entries, AgentText{Text: "still looking"})
	entries = ApplyToolActivity(entries, kagenta2a.ToolActivity{Kind: kagenta2a.ToolResultKind, ID: "c1", Name: "get_pods", Response: "ok"})
	entries = ApplyToolActivity(entries, kagenta2a.ToolActivity{Kind: kagenta2a.ToolResultKind, ID: "c9", Name: "orphan", Response: "x"})

	assert.Equal(t, []Entry{
		AgentText{Text: "looking"},
		ToolActivity{ID: "c1", Name: "get_pods", Args: map[string]any{"n": 1}, Outcome: Returned{Response: "ok"}},
		AgentText{Text: "still looking"},
		ToolActivity{ID: "c9", Name: "orphan", Outcome: Returned{Response: "x"}},
	}, entries)
}
