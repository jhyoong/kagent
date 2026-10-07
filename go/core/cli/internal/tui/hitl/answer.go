package hitl

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/transcript"
)

// QuestionKind is how a question is answered.
type QuestionKind int

const (
	// FreeText is a question without choices: the agent wants prose.
	FreeText QuestionKind = iota
	// OneChoice picks exactly one of the offered choices.
	OneChoice
	// AnyChoices picks one or more of the offered choices.
	AnyChoices
)

// KindOf is how question is answered. As in the web form, choices are the only
// answers when offered; without them the answer is prose, whatever Multiple says.
func KindOf(question kagenta2a.HITLQuestion) QuestionKind {
	switch {
	case len(question.Choices) == 0:
		return FreeText
	case question.Multiple:
		return AnyChoices
	default:
		return OneChoice
	}
}

// AnswerForm answers a pending ask-user request one question at a time, then
// reviews them when there are several. It is built from a Pending, so an answer
// always correlates with the request the task asked.
type AnswerForm struct {
	taskID a2atype.TaskID
	req    AskUser
	// answers are positional, as the runtime pairs them: answers[i] answers question i.
	answers   [][]string
	current   int
	reviewing bool
}

// NewAnswerForm starts an unanswered form at the first question of a pending ask-user request.
func NewAnswerForm(pending Pending) (*AnswerForm, bool) {
	req, ok := pending.Request.(AskUser)
	if !ok {
		return nil, false
	}
	return &AnswerForm{taskID: pending.TaskID, req: req, answers: make([][]string, len(req.Questions))}, true
}

// Request is what the form answers.
func (f *AnswerForm) Request() AskUser { return f.req }

// Questions are the questions in the order asked.
func (f *AnswerForm) Questions() []kagenta2a.HITLQuestion { return f.req.Questions }

// Current is the question being answered; while reviewing, the last one.
func (f *AnswerForm) Current() int { return f.current }

// Reviewing reports whether every question has been through and the answers are shown for sending.
func (f *AnswerForm) Reviewing() bool { return f.reviewing }

// Kind is how question i is answered.
func (f *AnswerForm) Kind(i int) QuestionKind {
	if !f.valid(i) {
		return FreeText
	}
	return KindOf(f.req.Questions[i])
}

// Selections is question i's answer so far.
func (f *AnswerForm) Selections(i int) []string {
	if !f.valid(i) {
		return nil
	}
	return slices.Clone(f.answers[i])
}

// Selected reports whether choice is part of question i's answer.
func (f *AnswerForm) Selected(i int, choice string) bool {
	return f.valid(i) && slices.Contains(f.answers[i], choice)
}

// Answered reports whether question i has an answer; an empty answer is none.
func (f *AnswerForm) Answered(i int) bool { return f.valid(i) && len(f.answers[i]) > 0 }

// Complete reports whether every question is answered: a gap would answer the wrong question.
func (f *AnswerForm) Complete() bool {
	for i := range f.answers {
		if !f.Answered(i) {
			return false
		}
	}
	return true
}

// Ready reports whether the answers can be sent now: complete and reviewed,
// or complete for a single question, which has nothing to review.
func (f *AnswerForm) Ready() bool {
	return f.Complete() && (f.reviewing || len(f.answers) == 1)
}

// Choose makes choice the current question's answer. It fits only a OneChoice
// question and only a choice it offers.
func (f *AnswerForm) Choose(choice string) bool {
	if !f.editing(OneChoice) || !slices.Contains(f.req.Questions[f.current].Choices, choice) {
		return false
	}
	f.answers[f.current] = []string{choice}
	return true
}

// Toggle adds or removes choice from the current question's answer, which
// keeps the order the choices were offered in. It fits only an AnyChoices question.
func (f *AnswerForm) Toggle(choice string) bool {
	if !f.editing(AnyChoices) {
		return false
	}
	choices := f.req.Questions[f.current].Choices
	if !slices.Contains(choices, choice) {
		return false
	}
	answer := f.answers[f.current]
	selected := !slices.Contains(answer, choice)
	toggled := make([]string, 0, len(answer)+1)
	for _, offered := range choices {
		keep := slices.Contains(answer, offered)
		if offered == choice {
			keep = selected
		}
		if keep && !slices.Contains(toggled, offered) { // a repeated choice is one selection
			toggled = append(toggled, offered)
		}
	}
	f.answers[f.current] = toggled
	return true
}

// SetText makes text, trimmed, the current question's answer; blank text is no
// answer. It fits only a FreeText question.
func (f *AnswerForm) SetText(text string) bool {
	if !f.editing(FreeText) {
		return false
	}
	if text = strings.TrimSpace(text); text == "" {
		f.answers[f.current] = nil
	} else {
		f.answers[f.current] = []string{text}
	}
	return true
}

// Next moves past an answered question: to the following one, or to review
// after the last of several. It reports whether it moved.
func (f *AnswerForm) Next() bool {
	if f.reviewing || !f.Answered(f.current) {
		return false
	}
	switch {
	case f.current < len(f.answers)-1:
		f.current++
	case len(f.answers) > 1:
		f.reviewing = true
	default:
		return false
	}
	return true
}

// Prev returns to the previous question, or from review to the last one,
// keeping every answer. It reports whether it moved.
func (f *AnswerForm) Prev() bool {
	switch {
	case f.reviewing:
		f.reviewing = false
	case f.current > 0:
		f.current--
	default:
		return false
	}
	return true
}

// Answer is the message that resumes the task, and the record the transcript
// shows for it. The message carries fallback prose for the agent and the
// structured answers under the HITL extension, which the runtime acts on.
func (f *AnswerForm) Answer(contextID string) (*a2atype.Message, transcript.AnswerRecord, error) {
	if !f.Complete() {
		return nil, transcript.AnswerRecord{}, errors.New("every question needs an answer before the answers are sent")
	}
	answers := make([][]string, len(f.answers))
	wire := make([]kagenta2a.AskUserAnswer, len(f.answers))
	for i, answer := range f.answers {
		answers[i] = slices.Clone(answer)
		wire[i] = kagenta2a.AskUserAnswer{Answer: slices.Clone(answer)}
	}
	response := &kagenta2a.AskUserResponse{Type: kagenta2a.HITLTypeAskUserResponse, ID: f.req.request.ID, Answers: wire}
	if err := kagenta2a.ValidateAskUserResponse(f.req.request, response); err != nil {
		return nil, transcript.AnswerRecord{}, fmt.Errorf("failed to build answers for task %s: %w", f.taskID, err)
	}

	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart(answerText(answers)))
	message.TaskID, message.ContextID = f.taskID, contextID
	if err := kagenta2a.AttachHITL(message, response); err != nil {
		return nil, transcript.AnswerRecord{}, fmt.Errorf("failed to attach answers for task %s: %w", f.taskID, err)
	}
	return message, transcript.AnswerRecord{Questions: f.req.Questions, Answers: answers, AskedBy: f.req.AskedBy}, nil
}

// editing reports whether the current question is being answered and is of kind.
func (f *AnswerForm) editing(kind QuestionKind) bool {
	return !f.reviewing && f.Kind(f.current) == kind
}

func (f *AnswerForm) valid(i int) bool { return i >= 0 && i < len(f.answers) }

// answerText is the prose beside the answers, as the web UI writes it: one
// line per answer, its selections joined by commas.
func answerText(answers [][]string) string {
	lines := make([]string, 0, len(answers))
	for _, answer := range answers {
		if line := strings.Join(answer, ", "); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}
