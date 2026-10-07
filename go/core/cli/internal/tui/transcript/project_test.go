package transcript

import (
	"encoding/json"
	"testing"

	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/hitl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"trpc.group/trpc-go/trpc-a2a-go/protocol"
)

// decode reads JSON into v the way the A2A client does.
func decode(t *testing.T, v any, raw any) {
	t.Helper()
	data, err := json.Marshal(raw)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, v))
}

func message(t *testing.T, id, role string, parts ...map[string]any) protocol.Message {
	t.Helper()
	var m protocol.Message
	decode(t, &m, map[string]any{"kind": "message", "messageId": id, "role": role, "parts": parts})
	return m
}

func text(s string) map[string]any { return map[string]any{"kind": "text", "text": s} }

func fnCall(name, id string, args map[string]any) map[string]any {
	return map[string]any{
		"kind":     "data",
		"data":     map[string]any{"name": name, "id": id, "args": args},
		"metadata": map[string]any{"kagent_type": "function_call"},
	}
}

func fnResponse(name, id string, response any) map[string]any {
	return map[string]any{
		"kind":     "data",
		"data":     map[string]any{"name": name, "id": id, "response": response},
		"metadata": map[string]any{"adk_type": "function_response"},
	}
}

func confirmation(id string, original map[string]any) map[string]any {
	return map[string]any{
		"kind": "data",
		"data": map[string]any{
			"name": "adk_request_confirmation",
			"id":   id,
			"args": map[string]any{"originalFunctionCall": original, "toolConfirmation": map[string]any{"confirmed": false}},
		},
		"metadata": map[string]any{"kagent_type": "function_call", "kagent_is_long_running": true},
	}
}

func decisionPart(data map[string]any) map[string]any {
	return map[string]any{"kind": "data", "data": data, "metadata": map[string]any{}}
}

func TestLogStreamsText(t *testing.T) {
	log := NewLog()
	log.AddUserMessage("hi")
	log.ApplyMessage(message(t, "p1", "agent", text("Hel")), true)
	log.ApplyMessage(message(t, "p2", "agent", text("lo")), true)
	assert.Equal(t, []Entry{UserMessage{Text: "hi"}, AgentText{Text: "Hello"}}, log.Entries())

	log.ApplyMessage(message(t, "w1", "agent", text("Hello there.")), false)
	assert.Equal(t, []Entry{UserMessage{Text: "hi"}, AgentText{Text: "Hello there."}}, log.Entries(), "whole text replaces its chunks")

	log.ApplyArtifact(message(t, "a", "agent", text("Hello there.\n")).Parts)
	assert.Len(t, log.Entries(), 2, "the final artifact echoes the last text")

	log.ApplyArtifact(message(t, "a2", "agent", text("Something new")).Parts)
	assert.Equal(t, AgentText{Text: "Something new"}, log.Entries()[2], "an artifact with new text is shown")
}

func TestLogToolActivity(t *testing.T) {
	log := NewLog()
	log.AddUserMessage("restart it")
	log.ApplyMessage(message(t, "1", "agent",
		text("Looking."),
		fnCall("k8s_get_pods", "c1", map[string]any{"ns": "shop"}),
		fnCall("k8s_delete_pod", "c2", map[string]any{"pod": "p"}),
	), false)
	log.ApplyMessage(message(t, "2", "agent",
		fnResponse("k8s_get_pods", "c1", map[string]any{"result": "pod p"}),
		fnResponse("k8s_delete_pod", "c2", map[string]any{"status": "confirmation_requested", "tool": "k8s_delete_pod"}),
	), false)
	log.ApplyMessage(message(t, "3", "agent", confirmation("conf", map[string]any{"name": "k8s_delete_pod", "id": "c2"})), false)

	assert.Equal(t, []Entry{
		UserMessage{Text: "restart it"},
		AgentText{Text: "Looking."},
		ToolActivity{ID: "c1", Name: "k8s_get_pods", Args: map[string]any{"ns": "shop"}, Outcome: Returned{Response: map[string]any{"result": "pod p"}}},
		ToolActivity{ID: "c2", Name: "k8s_delete_pod", Args: map[string]any{"pod": "p"}, Outcome: AwaitingApproval{}},
	}, log.Entries(), "the confirmation call is not a tool of the agent's")

	log.ApplyMessage(message(t, "4", "agent", fnResponse("k8s_delete_pod", "c2", map[string]any{"result": "Tool call was rejected by user. Reason: no"})), false)
	assert.Equal(t, NotRun{}, log.Entries()[3].(ToolActivity).Outcome, "the result settles the held call")

	log.AddUserMessage("again")
	log.ApplyMessage(message(t, "5", "agent", fnResponse("k8s_get_pods", "c1", map[string]any{"result": "x"})), false)
	assert.Len(t, log.Entries(), 6, "a call ID from an earlier turn does not pair")
}

func TestLogHidesQuestionTool(t *testing.T) {
	log := NewLog()
	log.ApplyMessage(message(t, "1", "agent",
		fnCall("ask_user", "q", map[string]any{"questions": []any{}}),
		fnResponse("ask_user", "q", map[string]any{"status": "pending"}),
		fnCall("adk_request_credential", "cred", nil),
	), false)
	assert.Empty(t, log.Entries())
}

func TestLogApplyTask(t *testing.T) {
	var answered, pending protocol.Task
	decode(t, &answered, map[string]any{
		"kind": "task", "id": "t1", "contextId": "s",
		"status": map[string]any{"state": "completed"},
		"history": []any{
			message(t, "u1", "user", text("clean up")),
			message(t, "a1", "agent", fnCall("k8s_delete_pod", "c1", nil), fnCall("k8s_scale", "c2", nil)),
			message(t, "a2", "agent", fnResponse("k8s_delete_pod", "c1", map[string]any{"status": "confirmation_requested"})),
			message(t, "a3", "agent",
				confirmation("conf1", map[string]any{"name": "k8s_delete_pod", "id": "c1"}),
				confirmation("conf2", map[string]any{"name": "k8s_scale", "id": "c2"}),
			),
			message(t, "u2", "user",
				decisionPart(map[string]any{"decision_type": "batch", "decisions": map[string]any{"c1": "approve", "c2": "reject"}, "rejection_reasons": map[string]any{"c2": "no"}}),
				text("Batch decision: 1 approved, 1 rejected"),
			),
			message(t, "a4", "agent", fnResponse("k8s_delete_pod", "c1", map[string]any{"result": "deleted"})),
			message(t, "a5", "agent", text("Done.")),
		},
	})
	decode(t, &pending, map[string]any{
		"kind": "task", "id": "t2", "contextId": "s",
		"status": map[string]any{"state": "input-required"},
		"history": []any{
			message(t, "a5", "agent", text("Done.")), // repeated across tasks
			message(t, "u3", "user", text("ask me")),
			message(t, "a6", "agent", confirmation("conf3", map[string]any{"name": "ask_user", "id": "q", "args": map[string]any{"questions": []any{}}})),
		},
	})

	log := NewLog()
	log.ApplyTask(&answered)
	log.ApplyTask(&pending)
	assert.Equal(t, []Entry{
		UserMessage{Text: "clean up"},
		ToolActivity{ID: "c1", Name: "k8s_delete_pod", Outcome: Returned{Response: map[string]any{"result": "deleted"}}},
		ToolActivity{ID: "c2", Name: "k8s_scale", Outcome: Running{}},
		ApprovalRecord{
			Tools:    []hitl.Tool{{ID: "c1", Name: "k8s_delete_pod"}, {ID: "c2", Name: "k8s_scale"}},
			Approved: []bool{true, false},
			Reasons:  []string{"", "no"},
		},
		AgentText{Text: "Done."},
		UserMessage{Text: "ask me"},
	}, log.Entries(), "decisions show as records; the pending request is left to the prompt")
}

func TestLogApplyTaskAnsweredQuestions(t *testing.T) {
	var task protocol.Task
	decode(t, &task, map[string]any{
		"kind": "task", "id": "t1", "contextId": "s",
		"status": map[string]any{"state": "completed"},
		"history": []any{
			message(t, "a1", "agent", confirmation("conf", map[string]any{"name": "ask_user", "id": "q", "args": map[string]any{
				"questions": []any{map[string]any{"question": "Size?", "choices": []any{"S", "L"}}},
			}})),
			message(t, "u1", "user", decisionPart(map[string]any{"decision_type": "approve", "ask_user_answers": []any{map[string]any{"answer": []any{"L"}}}}), text("Answered questions")),
		},
	})
	log := NewLog()
	log.ApplyTask(&task)
	assert.Equal(t, []Entry{AnswerRecord{
		Questions: []hitl.Question{{Question: "Size?", Choices: []string{"S", "L"}}},
		Answers:   [][]string{{"L"}},
	}}, log.Entries())
}

func TestRecordForDeclinedQuestions(t *testing.T) {
	record, ok := RecordFor(hitl.AskUser{Questions: []hitl.Question{{Question: "Size?"}}}, hitl.Decision{Type: hitl.Reject})
	require.True(t, ok)
	assert.True(t, record.(AnswerRecord).Declined)

	_, ok = RecordFor(hitl.Unknown{}, hitl.Decision{Type: hitl.Reject})
	assert.False(t, ok)
}

func TestClassifyResult(t *testing.T) {
	tests := []struct {
		name     string
		response any
		want     ToolOutcome
	}{
		{"held for approval", map[string]any{"status": "confirmation_requested"}, AwaitingApproval{}},
		{"question pending", map[string]any{"status": "pending"}, AwaitingApproval{}},
		{"rejected", map[string]any{"result": "Tool call was rejected by user."}, NotRun{}},
		{"rejected as text", "Tool call was rejected by user. Reason: x", NotRun{}},
		{"error", map[string]any{"isError": true, "content": "boom"}, Returned{Response: map[string]any{"isError": true, "content": "boom"}, Failed: true}},
		{"result", map[string]any{"result": "ok"}, Returned{Response: map[string]any{"result": "ok"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ClassifyResult(tt.response))
		})
	}
}

func TestIsPartial(t *testing.T) {
	assert.True(t, IsPartial(map[string]any{"adk_partial": true}))
	assert.True(t, IsPartial(map[string]any{"kagent_adk_partial": true}))
	assert.True(t, IsPartial(map[string]any{"kagent_partial": true}))
	assert.False(t, IsPartial(map[string]any{"kagent_adk_partial": false}))
	assert.False(t, IsPartial(nil))
}
