package hitl

import (
	"encoding/json"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/a2a"
	adka2a "github.com/kagent-dev/kagent/go/adk/pkg/a2a"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"trpc.group/trpc-go/trpc-a2a-go/protocol"
)

func twoTools() Pending {
	return Pending{TaskID: "task-1", Request: ToolApproval{Tools: []Tool{
		{ID: "call_1", Name: "k8s_delete_pod"},
		{ID: "call_2", Name: "k8s_scale"},
	}}}
}

// wireData is the decision data part of message, as JSON would carry it.
func wireData(t *testing.T, message protocol.Message) (map[string]any, string) {
	t.Helper()
	data, err := json.Marshal(message)
	require.NoError(t, err)
	var decoded struct {
		Kind      string `json:"kind"`
		MessageID string `json:"messageId"`
		TaskID    string `json:"taskId"`
		ContextID string `json:"contextId"`
		Role      string `json:"role"`
		Parts     []struct {
			Kind string         `json:"kind"`
			Data map[string]any `json:"data"`
			Text string         `json:"text"`
		} `json:"parts"`
	}
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, "message", decoded.Kind)
	assert.NotEmpty(t, decoded.MessageID)
	assert.Equal(t, "task-1", decoded.TaskID, "a decision must name the paused task")
	assert.Equal(t, "ctx-1", decoded.ContextID)
	assert.Equal(t, "user", decoded.Role)
	require.Len(t, decoded.Parts, 2)
	assert.Equal(t, "data", decoded.Parts[0].Kind)
	assert.Equal(t, "text", decoded.Parts[1].Kind)
	return decoded.Parts[0].Data, decoded.Parts[1].Text
}

func TestApprovalFormAnswer(t *testing.T) {
	tests := []struct {
		name      string
		decide    func(f *ApprovalForm)
		wantData  map[string]any
		wantLabel string
	}{
		{
			name:      "all approved is a uniform approve",
			decide:    func(f *ApprovalForm) { f.ApproveAll() },
			wantData:  map[string]any{"decision_type": "approve"},
			wantLabel: "Approved",
		},
		{
			name:      "all rejected without reasons is a uniform reject",
			decide:    func(f *ApprovalForm) { f.Reject(0, ""); f.Reject(1, "  ") },
			wantData:  map[string]any{"decision_type": "reject"},
			wantLabel: "Rejected",
		},
		{
			name:   "a reason makes a batch",
			decide: func(f *ApprovalForm) { f.Reject(0, "not now"); f.Reject(1, "") },
			wantData: map[string]any{
				"decision_type":     "batch",
				"decisions":         map[string]any{"call_1": "reject", "call_2": "reject"},
				"rejection_reasons": map[string]any{"call_1": "not now"},
			},
			wantLabel: "Batch decision: 0 approved, 2 rejected",
		},
		{
			name:   "mixed is a batch",
			decide: func(f *ApprovalForm) { f.Approve(0); f.Reject(1, "") },
			wantData: map[string]any{
				"decision_type": "batch",
				"decisions":     map[string]any{"call_1": "approve", "call_2": "reject"},
			},
			wantLabel: "Batch decision: 1 approved, 1 rejected",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form, ok := NewApprovalForm(twoTools())
			require.True(t, ok)
			tt.decide(form)
			message, decision, err := form.Answer("ctx-1")
			require.NoError(t, err)
			data, label := wireData(t, message)
			assert.Equal(t, tt.wantData, data)
			assert.Equal(t, tt.wantLabel, label)

			read, ok := ReadDecision(message)
			require.True(t, ok)
			assert.Equal(t, decision.Type, read.Type)
		})
	}
}

func TestApprovalFormIncomplete(t *testing.T) {
	form, ok := NewApprovalForm(twoTools())
	require.True(t, ok)
	form.Approve(0)
	assert.False(t, form.Complete())
	assert.Equal(t, 1, form.Decided())
	_, _, err := form.Answer("ctx-1")
	assert.Error(t, err)

	form.Reject(1, "why")
	form.ApproveAll()
	assert.Equal(t, Approved, form.Verdict(1))
	assert.Empty(t, form.Reason(1), "approving drops the earlier reason")

	_, ok = NewApprovalForm(Pending{Request: AskUser{}})
	assert.False(t, ok)
}

func TestAnswerForm(t *testing.T) {
	pending := Pending{TaskID: "task-1", Request: AskUser{Questions: []Question{
		{Question: "Which region?", Choices: []string{"eu", "us"}},
		{Question: "Which environments?", Choices: []string{"dev", "staging", "prod"}, Multiple: true},
		{Question: "Anything else?"},
	}}}
	form, ok := NewAnswerForm(pending)
	require.True(t, ok)

	assert.False(t, form.Next(), "an unanswered question cannot be skipped")
	assert.False(t, form.Toggle("eu"), "toggle fits only a multiple-choice question")
	assert.False(t, form.Choose("asia"), "only offered choices")
	require.True(t, form.Choose("us"))
	require.True(t, form.Next())

	require.True(t, form.Toggle("prod"))
	require.True(t, form.Toggle("dev"))
	require.True(t, form.Toggle("staging"))
	require.True(t, form.Toggle("staging"))
	assert.Equal(t, []string{"dev", "prod"}, form.Selections(1), "selections keep the offered order")
	require.True(t, form.Next())

	assert.False(t, form.SetText("x") && form.Ready(), "three questions need review")
	require.True(t, form.SetText("  no  "))
	require.True(t, form.Next())
	assert.True(t, form.Reviewing())
	assert.True(t, form.Ready())

	require.True(t, form.Prev())
	assert.False(t, form.Reviewing())
	require.True(t, form.Next())

	message, decision, err := form.Answer("ctx-1")
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"us"}, {"dev", "prod"}, {"no"}}, decision.Answers)
	data, label := wireData(t, message)
	assert.Equal(t, "Answered questions", label)
	assert.Equal(t, map[string]any{
		"decision_type": "approve",
		"ask_user_answers": []any{
			map[string]any{"answer": []any{"us"}},
			map[string]any{"answer": []any{"dev", "prod"}},
			map[string]any{"answer": []any{"no"}},
		},
	}, data)
}

func TestDecline(t *testing.T) {
	message := twoTools().Decline("ctx-1")
	data, label := wireData(t, message)
	assert.Equal(t, map[string]any{"decision_type": "reject"}, data)
	assert.Equal(t, "Rejected", label)
}

func TestReadDecision(t *testing.T) {
	message := protocol.NewMessage(protocol.MessageRoleUser, []protocol.Part{
		protocol.NewTextPart("Batch"),
		protocol.NewDataPart(map[string]any{
			"decision_type":     "batch",
			"decisions":         map[string]any{"a": "approve", "b": "reject", "c": "bogus"},
			"rejection_reasons": map[string]any{"b": "no", "a": ""},
		}),
	})
	decision, ok := ReadDecision(message)
	require.True(t, ok)
	assert.Equal(t, Batch, decision.Type)
	assert.Equal(t, map[string]DecisionType{"a": Approve, "b": Reject}, decision.Verdicts)
	assert.Equal(t, map[string]string{"b": "no"}, decision.Reasons)
	assert.True(t, decision.Approved("a"))
	assert.False(t, decision.Approved("b"))
	assert.True(t, decision.Approved("c"), "a call missing from a batch is approved, as the runtime does")
	assert.Equal(t, "no", decision.Reason("b"))

	reject, ok := ReadDecision(protocol.NewMessage(protocol.MessageRoleUser, []protocol.Part{
		protocol.NewDataPart(map[string]any{"decision_type": "reject", "rejection_reason": "later"}),
	}))
	require.True(t, ok)
	assert.Equal(t, "later", reject.Reason("anything"))

	assert.False(t, IsDecision(protocol.NewMessage(protocol.MessageRoleUser, []protocol.Part{protocol.NewTextPart("hi")})))
	assert.False(t, IsDecision(protocol.NewMessage(protocol.MessageRoleUser, []protocol.Part{protocol.NewDataPart(map[string]any{"decision_type": "maybe"})})))
}

// TestDecisionsResumeGoADK sends the TUI's decisions through the Go runtime's
// resume parser, so the wire format is checked against the server, not a copy of it.
func TestDecisionsResumeGoADK(t *testing.T) {
	direct := []map[string]any{
		confirmationJSON(t, "kagent_", "conf_1", callJSON("k8s_delete_pod", "call_1", nil), nil),
		confirmationJSON(t, "kagent_", "conf_2", callJSON("k8s_scale", "call_2", nil), nil),
	}
	askUser := []map[string]any{confirmationJSON(t, "kagent_", "conf_q", callJSON("ask_user", "call_q", askUserArgs()), nil)}
	nested := []map[string]any{confirmationJSON(t, "kagent_", "conf_outer", callJSON("k8s_agent", "call_outer", nil), map[string]any{
		"task_id": "sub-task", "subagent_name": "k8s_agent",
		"hitl_parts": []any{
			map[string]any{"name": "adk_request_confirmation", "id": "inner_1", "originalFunctionCall": callJSON("delete", "inner_call_1", nil)},
			map[string]any{"name": "adk_request_confirmation", "id": "inner_2", "originalFunctionCall": callJSON("scale", "inner_call_2", nil)},
		},
	})}

	type confirmed struct {
		Confirmed bool           `json:"confirmed"`
		Payload   map[string]any `json:"payload"`
	}
	tests := []struct {
		name    string
		request []map[string]any
		answer  func(t *testing.T, pending Pending) protocol.Message
		want    map[string]confirmed // by confirmation ID
	}{
		{
			name:    "mixed batch with a reason",
			request: direct,
			answer: func(t *testing.T, pending Pending) protocol.Message {
				form, ok := NewApprovalForm(pending)
				require.True(t, ok)
				form.Approve(0)
				form.Reject(1, "too risky")
				message, _, err := form.Answer("ctx-1")
				require.NoError(t, err)
				return message
			},
			want: map[string]confirmed{
				"conf_1": {Confirmed: true},
				"conf_2": {Confirmed: false, Payload: map[string]any{"rejection_reason": "too risky"}},
			},
		},
		{
			name:    "decline rejects everything",
			request: direct,
			answer: func(t *testing.T, pending Pending) protocol.Message {
				return pending.Decline("ctx-1")
			},
			want: map[string]confirmed{"conf_1": {Confirmed: false}, "conf_2": {Confirmed: false}},
		},
		{
			name:    "ask_user answers",
			request: askUser,
			answer: func(t *testing.T, pending Pending) protocol.Message {
				form, ok := NewAnswerForm(pending)
				require.True(t, ok)
				form.Choose("eu")
				form.Next()
				form.Toggle("prod")
				form.Next()
				form.SetText("nothing")
				form.Next()
				message, _, err := form.Answer("ctx-1")
				require.NoError(t, err)
				return message
			},
			want: map[string]confirmed{"conf_q": {Confirmed: true, Payload: map[string]any{"answers": []any{
				map[string]any{"answer": []any{"eu"}},
				map[string]any{"answer": []any{"prod"}},
				map[string]any{"answer": []any{"nothing"}},
			}}}},
		},
		{
			name:    "subagent batch carries inner decisions",
			request: nested,
			answer: func(t *testing.T, pending Pending) protocol.Message {
				form, ok := NewApprovalForm(pending)
				require.True(t, ok)
				form.Approve(0)
				form.Reject(1, "")
				message, _, err := form.Answer("ctx-1")
				require.NoError(t, err)
				return message
			},
			want: map[string]confirmed{"conf_outer": {Confirmed: false, Payload: map[string]any{
				"task_id": "sub-task", "subagent_name": "k8s_agent",
				"batch_decisions": map[string]any{"inner_call_1": "approve", "inner_call_2": "reject"},
			}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts := partsFrom(t, tt.request...)
			request, ok := ReadRequest(parts)
			require.True(t, ok)
			pending := Pending{TaskID: "task-1", Request: request}
			message := tt.answer(t, pending)

			stored := storedTask(t, tt.request)
			incoming := toA2A(t, message)
			resumed := adka2a.BuildResumeHITLMessage(stored, incoming)
			require.NotNil(t, resumed, "the Go runtime did not recognise the decision")

			got := map[string]confirmed{}
			for _, part := range resumed.Parts {
				data := part.(a2atype.DataPart).Data
				response := data["response"].(map[string]any)["response"].(string)
				var c confirmed
				require.NoError(t, json.Unmarshal([]byte(response), &c))
				if c.Payload != nil {
					delete(c.Payload, "hitl_parts") // carried through unchanged
				}
				got[data["id"].(string)] = c
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func storedTask(t *testing.T, parts []map[string]any) *a2atype.Task {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"kind": "task", "id": "task-1", "contextId": "ctx-1",
		"status": map[string]any{
			"state":   "input-required",
			"message": map[string]any{"kind": "message", "messageId": "m", "role": "agent", "parts": parts},
		},
	})
	require.NoError(t, err)
	var task a2atype.Task
	require.NoError(t, json.Unmarshal(data, &task))
	return &task
}

func toA2A(t *testing.T, message protocol.Message) *a2atype.Message {
	t.Helper()
	data, err := json.Marshal(message)
	require.NoError(t, err)
	var decoded a2atype.Message
	require.NoError(t, json.Unmarshal(data, &decoded))
	return &decoded
}
