package hitl

import (
	"fmt"

	"trpc.group/trpc-go/trpc-a2a-go/protocol"
)

// Wire keys of a decision's data part.
const (
	keyDecisionType     = "decision_type"
	keyDecisions        = "decisions"
	keyRejectionReasons = "rejection_reasons"
	keyRejectionReason  = "rejection_reason"
	keyAskUserAnswers   = "ask_user_answers"
)

// DecisionType is how a decision applies to the pending calls.
type DecisionType string

const (
	// Approve approves every pending call (or, with answers, answers the questions).
	Approve DecisionType = "approve"
	// Reject rejects every pending call, and declines questions.
	Reject DecisionType = "reject"
	// Batch decides each call on its own.
	Batch DecisionType = "batch"
)

// Decision is what the user sent for a paused task.
type Decision struct {
	Type DecisionType
	// Verdicts holds each call's decision, by tool ID; only for Batch.
	Verdicts map[string]DecisionType
	// Reasons holds rejection reasons by tool ID; "*" is a uniform rejection's reason.
	Reasons map[string]string
	// Answers are positional ask_user answers; only with Approve.
	Answers [][]string
}

// Approved reports whether the call with id was approved. A batch without a
// verdict for it approves it, as the runtime does.
func (d Decision) Approved(id string) bool {
	if d.Type != Batch {
		return d.Type == Approve
	}
	verdict, ok := d.Verdicts[id]
	return !ok || verdict == Approve
}

// Reason is the rejection reason for the call with id, if any.
func (d Decision) Reason(id string) string {
	if d.Type == Reject {
		return d.Reasons["*"]
	}
	return d.Reasons[id]
}

// data is the decision as its wire data part.
func (d Decision) data() map[string]any {
	data := map[string]any{keyDecisionType: string(d.Type)}
	if d.Type == Batch {
		verdicts := make(map[string]any, len(d.Verdicts))
		for id, verdict := range d.Verdicts {
			verdicts[id] = string(verdict)
		}
		data[keyDecisions] = verdicts
		if len(d.Reasons) > 0 {
			reasons := make(map[string]any, len(d.Reasons))
			for id, reason := range d.Reasons {
				reasons[id] = reason
			}
			data[keyRejectionReasons] = reasons
		}
	}
	if d.Type == Reject && d.Reasons["*"] != "" {
		data[keyRejectionReason] = d.Reasons["*"]
	}
	if len(d.Answers) > 0 {
		answers := make([]any, len(d.Answers))
		for i, answer := range d.Answers {
			values := make([]any, len(answer))
			for j, value := range answer {
				values[j] = value
			}
			answers[i] = map[string]any{"answer": values}
		}
		data[keyAskUserAnswers] = answers
	}
	return data
}

// Message is the user message that sends decision to the paused task, with label
// as the readable text beside it.
func (p Pending) Message(contextID string, decision Decision, label string) protocol.Message {
	taskID := p.TaskID
	message := protocol.NewMessageWithContext(protocol.MessageRoleUser, []protocol.Part{
		protocol.DataPart{Kind: protocol.KindData, Data: decision.data(), Metadata: map[string]any{}},
		protocol.NewTextPart(label),
	}, &taskID, &contextID)
	return message
}

// Decline is the decision that gives up the request: it rejects every pending
// call, and an ask_user call answers that the user declined.
func (p Pending) Decline(contextID string) protocol.Message {
	return p.Message(contextID, Decision{Type: Reject}, "Rejected")
}

// ReadDecision reads the decision a user message carries.
func ReadDecision(message protocol.Message) (Decision, bool) {
	for _, part := range message.Parts {
		dataPart := asDataPart(part)
		if dataPart == nil {
			continue
		}
		data, ok := dataPart.Data.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := data[keyDecisionType].(string)
		decision := Decision{Type: DecisionType(kind)}
		switch decision.Type {
		case Approve, Reject, Batch:
		default:
			continue
		}
		if verdicts, ok := data[keyDecisions].(map[string]any); ok {
			decision.Verdicts = map[string]DecisionType{}
			for id, raw := range verdicts {
				if verdict, _ := raw.(string); verdict == string(Approve) || verdict == string(Reject) {
					decision.Verdicts[id] = DecisionType(verdict)
				}
			}
		}
		decision.Reasons = stringMap(data[keyRejectionReasons])
		if reason, _ := data[keyRejectionReason].(string); reason != "" {
			if decision.Reasons == nil {
				decision.Reasons = map[string]string{}
			}
			decision.Reasons["*"] = reason
		}
		answers, _ := data[keyAskUserAnswers].([]any)
		for _, raw := range answers {
			fields, _ := raw.(map[string]any)
			values, _ := fields["answer"].([]any)
			answer := make([]string, 0, len(values))
			for _, value := range values {
				if text, ok := value.(string); ok {
					answer = append(answer, text)
				}
			}
			decision.Answers = append(decision.Answers, answer)
		}
		return decision, true
	}
	return Decision{}, false
}

// IsDecision reports whether message carries a decision; such messages are
// shown as records, not as user prose.
func IsDecision(message protocol.Message) bool {
	_, ok := ReadDecision(message)
	return ok
}

// approvalDecision encodes per-tool verdicts as the web UI does: uniform when
// every call agrees and no reason needs carrying, a batch otherwise.
func approvalDecision(tools []Tool, approved []bool, reasons []string) Decision {
	allApproved, allRejected, anyReason := true, true, false
	for i := range tools {
		allApproved = allApproved && approved[i]
		allRejected = allRejected && !approved[i]
		anyReason = anyReason || (!approved[i] && reasons[i] != "")
	}
	switch {
	case allApproved:
		return Decision{Type: Approve}
	case allRejected && !anyReason:
		return Decision{Type: Reject}
	}
	decision := Decision{Type: Batch, Verdicts: make(map[string]DecisionType, len(tools))}
	for i, tool := range tools {
		if approved[i] {
			decision.Verdicts[tool.ID] = Approve
			continue
		}
		decision.Verdicts[tool.ID] = Reject
		if reasons[i] != "" {
			if decision.Reasons == nil {
				decision.Reasons = map[string]string{}
			}
			decision.Reasons[tool.ID] = reasons[i]
		}
	}
	return decision
}

// approvalLabel is the text beside an approval decision, as the web UI writes it.
func approvalLabel(decision Decision, approved []bool) string {
	switch decision.Type {
	case Approve:
		return "Approved"
	case Reject:
		return "Rejected"
	}
	yes := 0
	for _, a := range approved {
		if a {
			yes++
		}
	}
	return fmt.Sprintf("Batch decision: %d approved, %d rejected", yes, len(approved)-yes)
}

func stringMap(raw any) map[string]string {
	fields, ok := raw.(map[string]any)
	if !ok || len(fields) == 0 {
		return nil
	}
	result := make(map[string]string, len(fields))
	for key, value := range fields {
		if text, _ := value.(string); text != "" {
			result[key] = text
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
