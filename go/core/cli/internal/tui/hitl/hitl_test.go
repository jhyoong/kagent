package hitl

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"trpc.group/trpc-go/trpc-a2a-go/protocol"
)

// confirmationJSON is one adk_request_confirmation part as the runtimes emit it,
// with metadata prefixed as prefix ("kagent_" or "adk_").
func confirmationJSON(t *testing.T, prefix, id string, original map[string]any, payload map[string]any) map[string]any {
	t.Helper()
	return map[string]any{
		"kind": "data",
		"data": map[string]any{
			"name": "adk_request_confirmation",
			"id":   id,
			"args": map[string]any{
				"originalFunctionCall": original,
				"toolConfirmation": map[string]any{
					"hint":      "Tool '" + original["name"].(string) + "' requires approval before execution.",
					"confirmed": false,
					"payload":   payload,
				},
			},
		},
		"metadata": map[string]any{prefix + "type": "function_call", prefix + "is_long_running": true},
	}
}

func callJSON(name, id string, args map[string]any) map[string]any {
	return map[string]any{"name": name, "id": id, "args": args}
}

// partsFrom decodes raw parts the way the A2A client does.
func partsFrom(t *testing.T, raw ...map[string]any) []protocol.Part {
	t.Helper()
	data, err := json.Marshal(map[string]any{"kind": "message", "messageId": "m", "role": "agent", "parts": raw})
	require.NoError(t, err)
	var message protocol.Message
	require.NoError(t, json.Unmarshal(data, &message))
	return message.Parts
}

func askUserArgs() map[string]any {
	return map[string]any{"questions": []any{
		map[string]any{"question": "Which region?", "choices": []any{"eu", "us"}},
		map[string]any{"question": "Which environments?", "choices": []any{"dev", "prod"}, "multiple": true},
		map[string]any{"question": "Anything else?"},
	}}
}

func TestReadRequest(t *testing.T) {
	deletePod := callJSON("k8s_delete_pod", "call_1", map[string]any{"pod": "checkout"})
	scale := callJSON("k8s_scale", "call_2", map[string]any{"replicas": float64(0)})
	subagent := callJSON("kagent__shop__k8s_agent", "call_outer", map[string]any{"request": "clean up"})
	nested := func(inner ...map[string]any) map[string]any {
		parts := make([]any, len(inner))
		for i, c := range inner {
			parts[i] = map[string]any{"name": "adk_request_confirmation", "id": "inner_conf", "originalFunctionCall": c}
		}
		return map[string]any{"task_id": "sub-task", "context_id": "sub-ctx", "subagent_name": "k8s_agent", "hitl_parts": parts}
	}

	tests := []struct {
		name  string
		parts []map[string]any
		want  Request
		ok    bool
	}{
		{
			name:  "single tool approval",
			parts: []map[string]any{confirmationJSON(t, "kagent_", "conf_1", deletePod, nil)},
			want: ToolApproval{
				Tools: []Tool{{ID: "call_1", Name: "k8s_delete_pod", Args: map[string]any{"pod": "checkout"}}},
				Hint:  "Tool 'k8s_delete_pod' requires approval before execution.",
			},
			ok: true,
		},
		{
			name: "parallel tool approvals with adk_ metadata",
			parts: []map[string]any{
				confirmationJSON(t, "adk_", "conf_1", deletePod, nil),
				confirmationJSON(t, "adk_", "conf_2", scale, nil),
			},
			want: ToolApproval{Tools: []Tool{
				{ID: "call_1", Name: "k8s_delete_pod", Args: map[string]any{"pod": "checkout"}},
				{ID: "call_2", Name: "k8s_scale", Args: map[string]any{"replicas": float64(0)}},
			}},
			ok: true,
		},
		{
			name:  "subagent approval shows the inner calls",
			parts: []map[string]any{confirmationJSON(t, "kagent_", "conf_1", subagent, nested(deletePod, scale))},
			want: ToolApproval{
				Tools: []Tool{
					{ID: "call_1", Name: "k8s_delete_pod", Args: map[string]any{"pod": "checkout"}},
					{ID: "call_2", Name: "k8s_scale", Args: map[string]any{"replicas": float64(0)}},
				},
				Hint:     "Tool 'kagent__shop__k8s_agent' requires approval before execution.",
				Subagent: "k8s_agent",
			},
			ok: true,
		},
		{
			name:  "ask_user",
			parts: []map[string]any{confirmationJSON(t, "kagent_", "conf_1", callJSON("ask_user", "call_q", askUserArgs()), nil)},
			want: AskUser{Questions: []Question{
				{Question: "Which region?", Choices: []string{"eu", "us"}},
				{Question: "Which environments?", Choices: []string{"dev", "prod"}, Multiple: true},
				{Question: "Anything else?"},
			}},
			ok: true,
		},
		{
			name:  "subagent ask_user",
			parts: []map[string]any{confirmationJSON(t, "kagent_", "conf_1", subagent, nested(callJSON("ask_user", "call_q", askUserArgs())))},
			want: AskUser{
				Questions: []Question{
					{Question: "Which region?", Choices: []string{"eu", "us"}},
					{Question: "Which environments?", Choices: []string{"dev", "prod"}, Multiple: true},
					{Question: "Anything else?"},
				},
				Subagent: "k8s_agent",
			},
			ok: true,
		},
		{
			name: "no confirmation",
			parts: []map[string]any{
				{"kind": "text", "text": "hello"},
				{"kind": "data", "data": map[string]any{"name": "k8s_get_pods", "id": "c"}, "metadata": map[string]any{"kagent_type": "function_call"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ReadRequest(partsFrom(t, tt.parts...))
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReadRequestUnknown(t *testing.T) {
	tests := []struct {
		name  string
		parts []map[string]any
	}{
		{
			name: "questions pending with a tool",
			parts: []map[string]any{
				confirmationJSON(t, "kagent_", "conf_1", callJSON("ask_user", "call_q", askUserArgs()), nil),
				confirmationJSON(t, "kagent_", "conf_2", callJSON("k8s_delete_pod", "call_1", nil), nil),
			},
		},
		{
			name:  "tool without a call ID",
			parts: []map[string]any{confirmationJSON(t, "kagent_", "conf_1", callJSON("k8s_delete_pod", "", nil), nil)},
		},
		{
			name: "confirmation without its call",
			parts: []map[string]any{{
				"kind":     "data",
				"data":     map[string]any{"name": "adk_request_confirmation", "id": "conf_1", "args": map[string]any{}},
				"metadata": map[string]any{"kagent_type": "function_call", "kagent_is_long_running": true},
			}},
		},
		{
			name:  "malformed inner calls",
			parts: []map[string]any{confirmationJSON(t, "kagent_", "conf_1", callJSON("sub", "c", nil), map[string]any{"hitl_parts": "nope"})},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ReadRequest(partsFrom(t, tt.parts...))
			require.True(t, ok)
			unknown, isUnknown := got.(Unknown)
			require.True(t, isUnknown, "got %#v", got)
			assert.Error(t, unknown.Problem)
		})
	}
}

func TestLastPending(t *testing.T) {
	confirmation := partsFrom(t, confirmationJSON(t, "kagent_", "conf_1", callJSON("k8s_delete_pod", "call_1", nil), nil))
	paused := func(id string) *protocol.Task {
		return &protocol.Task{ID: id, Status: protocol.TaskStatus{
			State:   protocol.TaskStateInputRequired,
			Message: &protocol.Message{Role: protocol.MessageRoleAgent, Parts: confirmation},
		}}
	}
	done := &protocol.Task{ID: "done", Status: protocol.TaskStatus{State: protocol.TaskStateCompleted}}

	pending, ok := LastPending([]*protocol.Task{paused("old"), done, paused("new"), done})
	require.True(t, ok)
	assert.Equal(t, "new", pending.TaskID)

	_, ok = LastPending([]*protocol.Task{done, nil})
	assert.False(t, ok)

	working := paused("w")
	working.Status.State = protocol.TaskStateWorking
	_, ok = ReadPending(working)
	assert.False(t, ok, "only an input-required task is paused")
}
