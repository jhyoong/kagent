package hitl

import (
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/transcript"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const prose = "Human input is required before the agent can continue."

// statusMessage is a paused task's status message carrying payload under the HITL extension.
func statusMessage(t *testing.T, payload any) *a2atype.Message {
	t.Helper()
	message := a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart(prose))
	if payload != nil {
		require.NoError(t, kagenta2a.AttachHITL(message, payload))
	}
	return message
}

func pausedTask(state a2atype.TaskState, message *a2atype.Message) *a2atype.Task {
	return &a2atype.Task{ID: "task-1", ContextID: "ctx-1", Status: a2atype.TaskStatus{State: state, Message: message}}
}

var (
	deletePod = kagenta2a.HITLTool{ID: "approval-1", CallID: "call-1", Name: "k8s_delete_pod", Args: map[string]any{"pod": "checkout-7d9f"}}
	scale     = kagenta2a.HITLTool{ID: "approval-2", CallID: "call-2", Name: "k8s_scale", Args: map[string]any{"replicas": float64(0)}}
	parent    = kagenta2a.HITLTool{ID: "parent-1", CallID: "parent-call", Name: "billing-agent"}
)

func TestReadPending(t *testing.T) {
	direct := &kagenta2a.ToolApprovalRequest{
		Type: kagenta2a.HITLTypeToolApprovalRequest, Hint: "Deleting this pod restarts checkout",
		Tools: []kagenta2a.HITLTool{deletePod, scale},
	}
	nested := &kagenta2a.ToolApprovalRequest{
		Type: kagenta2a.HITLTypeToolApprovalRequest, Hint: "child asks",
		Tools:  []kagenta2a.HITLTool{parent},
		Nested: &kagenta2a.NestedHITLRequest{SubagentName: "billing-agent", TaskID: "child-task", Tools: []kagenta2a.HITLTool{deletePod}},
	}
	question := kagenta2a.HITLQuestion{Question: "Which namespace?", Choices: []string{"shop", "dev"}}
	askUser := &kagenta2a.AskUserRequest{Type: kagenta2a.HITLTypeAskUserRequest, ID: "ask-1", Questions: []kagenta2a.HITLQuestion{question}}

	tests := []struct {
		name    string
		task    *a2atype.Task
		want    Request
		problem bool
		notOK   bool
	}{
		{
			name: "direct tool approval decides the requested tools",
			task: pausedTask(a2atype.TaskStateInputRequired, statusMessage(t, direct)),
			want: ToolApproval{Tools: direct.Tools, Hint: direct.Hint, request: direct},
		},
		{
			name: "nested tool approval decides the child's tools",
			task: pausedTask(a2atype.TaskStateInputRequired, statusMessage(t, nested)),
			want: ToolApproval{Tools: nested.Nested.Tools, Hint: "child asks", AskedBy: "billing-agent", request: nested},
		},
		{
			name: "a tool approval without a hint shows the status prose",
			task: pausedTask(a2atype.TaskStateInputRequired, statusMessage(t, &kagenta2a.ToolApprovalRequest{
				Type: kagenta2a.HITLTypeToolApprovalRequest, Tools: []kagenta2a.HITLTool{deletePod},
			})),
			want: ToolApproval{Tools: []kagenta2a.HITLTool{deletePod}, Hint: prose, request: &kagenta2a.ToolApprovalRequest{
				Type: kagenta2a.HITLTypeToolApprovalRequest, Tools: []kagenta2a.HITLTool{deletePod},
			}},
		},
		{
			name: "direct ask_user",
			task: pausedTask(a2atype.TaskStateInputRequired, statusMessage(t, askUser)),
			want: AskUser{Questions: askUser.Questions, request: askUser},
		},
		{
			name: "nested ask_user is unknown until its correlation is settled",
			task: pausedTask(a2atype.TaskStateInputRequired, statusMessage(t, &kagenta2a.AskUserRequest{
				Type: kagenta2a.HITLTypeAskUserRequest, ID: "ask-1", Questions: []kagenta2a.HITLQuestion{question},
				Nested: &kagenta2a.NestedHITLRequest{SubagentName: "billing-agent", TaskID: "child", Tools: []kagenta2a.HITLTool{{ID: "child-ask", Name: "ask_user"}}},
			})),
			want: Unknown{Prose: prose},
		},
		{
			name: "an unknown request type",
			task: pausedTask(a2atype.TaskStateInputRequired, statusMessage(t, map[string]any{"type": "pick_a_file"})),
			want: Unknown{Prose: prose},
		},
		{
			name: "no extension is prose only",
			task: pausedTask(a2atype.TaskStateInputRequired, statusMessage(t, nil)),
			want: Unknown{Prose: prose},
		},
		{
			name: "no status message",
			task: pausedTask(a2atype.TaskStateInputRequired, nil),
			want: Unknown{},
		},
		{
			name: "a tool approval with no tools is malformed",
			task: pausedTask(a2atype.TaskStateInputRequired, statusMessage(t, map[string]any{
				"type": kagenta2a.HITLTypeToolApprovalRequest, "tools": []any{},
			})),
			problem: true,
		},
		{
			name: "a nested tool approval with no child tools is malformed",
			task: pausedTask(a2atype.TaskStateInputRequired, statusMessage(t, &kagenta2a.ToolApprovalRequest{
				Type: kagenta2a.HITLTypeToolApprovalRequest, Tools: []kagenta2a.HITLTool{parent},
				Nested: &kagenta2a.NestedHITLRequest{SubagentName: "billing-agent"},
			})),
			problem: true,
		},
		{
			name: "a tool without an id cannot be decided",
			task: pausedTask(a2atype.TaskStateInputRequired, statusMessage(t, &kagenta2a.ToolApprovalRequest{
				Type: kagenta2a.HITLTypeToolApprovalRequest, Tools: []kagenta2a.HITLTool{{Name: "k8s_scale"}},
			})),
			problem: true,
		},
		{
			name: "a repeated tool id cannot be decided",
			task: pausedTask(a2atype.TaskStateInputRequired, statusMessage(t, &kagenta2a.ToolApprovalRequest{
				Type: kagenta2a.HITLTypeToolApprovalRequest, Tools: []kagenta2a.HITLTool{deletePod, deletePod},
			})),
			problem: true,
		},
		{
			name: "an ask_user without an id is malformed",
			task: pausedTask(a2atype.TaskStateInputRequired, statusMessage(t, map[string]any{
				"type": kagenta2a.HITLTypeAskUserRequest, "questions": []any{map[string]any{"question": "?"}},
			})),
			problem: true,
		},
		{
			name: "an ask_user without questions cannot be answered",
			task: pausedTask(a2atype.TaskStateInputRequired, statusMessage(t, &kagenta2a.AskUserRequest{
				Type: kagenta2a.HITLTypeAskUserRequest, ID: "ask-1",
			})),
			problem: true,
		},
		{name: "a working task is not pending", task: pausedTask(a2atype.TaskStateWorking, statusMessage(t, direct)), notOK: true},
		{name: "auth required is not answerable here", task: pausedTask(a2atype.TaskStateAuthRequired, statusMessage(t, direct)), notOK: true},
		{name: "no task", notOK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pending, ok := ReadPending(tt.task)
			if tt.notOK {
				assert.False(t, ok)
				return
			}
			require.True(t, ok)
			assert.Equal(t, a2atype.TaskID("task-1"), pending.TaskID)
			if tt.problem {
				unknown, isUnknown := pending.Request.(Unknown)
				require.True(t, isUnknown, "got %T", pending.Request)
				assert.Error(t, unknown.Problem)
				assert.Equal(t, prose, unknown.Prose)
				return
			}
			assert.Equal(t, tt.want, pending.Request)
		})
	}
}

func approvalPending(t *testing.T, request *kagenta2a.ToolApprovalRequest) Pending {
	t.Helper()
	pending, ok := ReadPending(pausedTask(a2atype.TaskStateInputRequired, statusMessage(t, request)))
	require.True(t, ok)
	return pending
}

func directPending(t *testing.T) Pending {
	return approvalPending(t, &kagenta2a.ToolApprovalRequest{
		Type: kagenta2a.HITLTypeToolApprovalRequest, Tools: []kagenta2a.HITLTool{deletePod, scale},
	})
}

func TestNewApprovalFormNeedsAToolApproval(t *testing.T) {
	_, ok := NewApprovalForm(Pending{TaskID: "task-1", Request: Unknown{Prose: prose}})
	assert.False(t, ok)

	form, ok := NewApprovalForm(directPending(t))
	require.True(t, ok)
	assert.Equal(t, []kagenta2a.HITLTool{deletePod, scale}, form.Tools())
	assert.Equal(t, 0, form.Decided())
	assert.False(t, form.Complete())
}

func TestApprovalFormTransitions(t *testing.T) {
	tests := []struct {
		name     string
		act      func(*ApprovalForm)
		verdicts []Verdict
		reasons  []string
		complete bool
	}{
		{
			name:     "one decision is not complete",
			act:      func(f *ApprovalForm) { f.Approve(0) },
			verdicts: []Verdict{Approved, Undecided}, reasons: []string{"", ""},
		},
		{
			name:     "approve all decides every tool",
			act:      func(f *ApprovalForm) { f.ApproveAll() },
			verdicts: []Verdict{Approved, Approved}, reasons: []string{"", ""}, complete: true,
		},
		{
			name:     "approve all overrides a rejection and drops its reason",
			act:      func(f *ApprovalForm) { f.Reject(1, "no"); f.ApproveAll() },
			verdicts: []Verdict{Approved, Approved}, reasons: []string{"", ""}, complete: true,
		},
		{
			name:     "a rejection keeps its trimmed reason",
			act:      func(f *ApprovalForm) { f.Approve(0); f.Reject(1, "  takes the shop down ") },
			verdicts: []Verdict{Approved, Rejected}, reasons: []string{"", "takes the shop down"}, complete: true,
		},
		{
			name:     "approving a rejected tool forgets its reason",
			act:      func(f *ApprovalForm) { f.Reject(0, "no"); f.Approve(0) },
			verdicts: []Verdict{Approved, Undecided}, reasons: []string{"", ""},
		},
		{
			name:     "an out of range index changes nothing",
			act:      func(f *ApprovalForm) { f.Approve(-1); f.Reject(2, "x") },
			verdicts: []Verdict{Undecided, Undecided}, reasons: []string{"", ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form, ok := NewApprovalForm(directPending(t))
			require.True(t, ok)
			tt.act(form)
			for i := range form.Tools() {
				assert.Equal(t, tt.verdicts[i], form.Verdict(i), "verdict %d", i)
				assert.Equal(t, tt.reasons[i], form.Reason(i), "reason %d", i)
			}
			assert.Equal(t, tt.complete, form.Complete())
		})
	}
}

func TestApprovalFormAnswer(t *testing.T) {
	nestedRequest := &kagenta2a.ToolApprovalRequest{
		Type: kagenta2a.HITLTypeToolApprovalRequest, Tools: []kagenta2a.HITLTool{parent},
		Nested: &kagenta2a.NestedHITLRequest{SubagentName: "billing-agent", TaskID: "child", Tools: []kagenta2a.HITLTool{deletePod, scale}},
	}
	tests := []struct {
		name     string
		request  *kagenta2a.ToolApprovalRequest
		act      func(*ApprovalForm)
		wantText string
		want     []kagenta2a.ToolApproval
		askedBy  string
	}{
		{
			name: "direct",
			request: &kagenta2a.ToolApprovalRequest{
				Type: kagenta2a.HITLTypeToolApprovalRequest, Tools: []kagenta2a.HITLTool{deletePod, scale},
			},
			act:      func(f *ApprovalForm) { f.Approve(0); f.Reject(1, "scaling to zero takes the shop down") },
			wantText: "Approved: k8s_delete_pod\nRejected: k8s_scale\nReason: scaling to zero takes the shop down",
			want: []kagenta2a.ToolApproval{
				{ID: "approval-1", Approved: true},
				{ID: "approval-2", RejectionReason: "scaling to zero takes the shop down"},
			},
		},
		{
			name:     "nested decides the child's tools",
			request:  nestedRequest,
			act:      func(f *ApprovalForm) { f.ApproveAll() },
			wantText: "Approved: k8s_delete_pod\nApproved: k8s_scale",
			want:     []kagenta2a.ToolApproval{{ID: "approval-1", Approved: true}, {ID: "approval-2", Approved: true}},
			askedBy:  "billing-agent",
		},
		{
			name: "a rejection without a reason",
			request: &kagenta2a.ToolApprovalRequest{
				Type: kagenta2a.HITLTypeToolApprovalRequest, Tools: []kagenta2a.HITLTool{deletePod},
			},
			act:      func(f *ApprovalForm) { f.Reject(0, "") },
			wantText: "Rejected: k8s_delete_pod",
			want:     []kagenta2a.ToolApproval{{ID: "approval-1"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form, ok := NewApprovalForm(approvalPending(t, tt.request))
			require.True(t, ok)
			tt.act(form)

			message, record, err := form.Answer("ctx-1")
			require.NoError(t, err)

			assert.Equal(t, a2atype.TaskID("task-1"), message.TaskID)
			assert.Equal(t, "ctx-1", message.ContextID)
			assert.Equal(t, a2atype.MessageRoleUser, message.Role)
			assert.Equal(t, []string{kagenta2a.HITLExtensionURI}, message.Extensions)
			assert.Equal(t, tt.wantText, message.Parts[0].Text())

			response, err := kagenta2a.ParseToolApprovalResponse(message)
			require.NoError(t, err)
			assert.Equal(t, tt.want, response.Approvals)
			assert.NoError(t, kagenta2a.ValidateToolApprovalResponse(tt.request, response), "the runtime accepts it")

			wantTools := tt.request.Tools
			if tt.request.Nested != nil {
				wantTools = tt.request.Nested.Tools
			}
			assert.Equal(t, transcript.ApprovalRecord{Tools: wantTools, Decisions: tt.want, AskedBy: tt.askedBy}, record)
		})
	}
}

func TestApprovalFormAnswerNeedsEveryDecision(t *testing.T) {
	form, ok := NewApprovalForm(directPending(t))
	require.True(t, ok)
	form.Approve(0)

	_, _, err := form.Answer("ctx-1")

	assert.Error(t, err)
}
