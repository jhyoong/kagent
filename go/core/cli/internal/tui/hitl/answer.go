package hitl

import (
	"errors"
	"slices"
	"strings"

	"trpc.group/trpc-go/trpc-a2a-go/protocol"
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

// KindOf is how question is answered: by its choices when it offers them, by
// prose otherwise, whatever Multiple says.
func KindOf(question Question) QuestionKind {
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
// always resumes the task that asked.
type AnswerForm struct {
	pending Pending
	req     AskUser
	// answers are positional, as the runtime pairs them: answers[i] answers question i.
	answers   [][]string
	current   int
	reviewing bool
}

// NewAnswerForm starts an unanswered form at the first question of a pending ask-user request.
func NewAnswerForm(pending Pending) (*AnswerForm, bool) {
	req, ok := pending.Request.(AskUser)
	if !ok || len(req.Questions) == 0 {
		return nil, false
	}
	return &AnswerForm{pending: pending, req: req, answers: make([][]string, len(req.Questions))}, true
}

// Request is what the form answers.
func (f *AnswerForm) Request() AskUser { return f.req }

// Questions are the questions in the order asked.
func (f *AnswerForm) Questions() []Question { return f.req.Questions }

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

// Answer is the message that resumes the task, and the decision it carries.
// Answers travel with an approve decision, as the runtime expects.
func (f *AnswerForm) Answer(contextID string) (protocol.Message, Decision, error) {
	if !f.Complete() {
		return protocol.Message{}, Decision{}, errors.New("every question needs an answer before the answers are sent")
	}
	answers := make([][]string, len(f.answers))
	for i, answer := range f.answers {
		answers[i] = slices.Clone(answer)
	}
	decision := Decision{Type: Approve, Answers: answers}
	return f.pending.Message(contextID, decision, "Answered questions"), decision, nil
}

// editing reports whether the current question is being answered and is of kind.
func (f *AnswerForm) editing(kind QuestionKind) bool {
	return !f.reviewing && f.Kind(f.current) == kind
}

func (f *AnswerForm) valid(i int) bool { return i >= 0 && i < len(f.answers) }
