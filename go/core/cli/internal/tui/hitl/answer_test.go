package hitl

import (
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/transcript"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	regionQuestion = kagenta2a.HITLQuestion{Question: "Which region?", Choices: []string{"eu-west-1", "us-east-1"}}
	envQuestion    = kagenta2a.HITLQuestion{Question: "Which environments?", Choices: []string{"staging", "production", "dev"}, Multiple: true}
	dbQuestion     = kagenta2a.HITLQuestion{Question: "Which database should we use for storage?"}
)

func askPending(t *testing.T, questions ...kagenta2a.HITLQuestion) (Pending, *kagenta2a.AskUserRequest) {
	t.Helper()
	request := &kagenta2a.AskUserRequest{Type: kagenta2a.HITLTypeAskUserRequest, ID: "ask-1", Questions: questions}
	pending, ok := ReadPending(pausedTask(a2atype.TaskStateInputRequired, statusMessage(t, request)))
	require.True(t, ok)
	return pending, request
}

func TestNewAnswerFormNeedsAnAskUser(t *testing.T) {
	_, ok := NewAnswerForm(directPending(t))
	assert.False(t, ok, "a tool approval is not a question")
	_, ok = NewAnswerForm(Pending{TaskID: "task-1", Request: Unknown{Prose: prose}})
	assert.False(t, ok)

	pending, _ := askPending(t, regionQuestion, envQuestion)
	form, ok := NewAnswerForm(pending)
	require.True(t, ok)
	assert.Equal(t, []kagenta2a.HITLQuestion{regionQuestion, envQuestion}, form.Questions())
	assert.Equal(t, 0, form.Current())
	assert.False(t, form.Reviewing())
	assert.False(t, form.Complete())
	assert.Equal(t, OneChoice, form.Kind(0))
	assert.Equal(t, AnyChoices, form.Kind(1))
}

func TestQuestionKind(t *testing.T) {
	assert.Equal(t, OneChoice, KindOf(regionQuestion))
	assert.Equal(t, AnyChoices, KindOf(envQuestion))
	assert.Equal(t, FreeText, KindOf(dbQuestion))
	assert.Equal(t, FreeText, KindOf(kagenta2a.HITLQuestion{Question: "Anything?", Multiple: true}), "no choices means prose, as in the web form")
}

func TestAnswerFormTransitions(t *testing.T) {
	type state struct {
		current   int
		reviewing bool
		answers   [][]string
		ready     bool
	}
	tests := []struct {
		name      string
		questions []kagenta2a.HITLQuestion
		act       func(t *testing.T, f *AnswerForm)
		want      state
	}{
		{
			name:      "next waits for an answer",
			questions: []kagenta2a.HITLQuestion{regionQuestion, envQuestion},
			act:       func(t *testing.T, f *AnswerForm) { assert.False(t, f.Next()) },
			want:      state{answers: [][]string{nil, nil}},
		},
		{
			name:      "choose replaces the single answer and next moves on",
			questions: []kagenta2a.HITLQuestion{regionQuestion, envQuestion},
			act: func(t *testing.T, f *AnswerForm) {
				assert.True(t, f.Choose("us-east-1"))
				assert.True(t, f.Choose("eu-west-1"))
				assert.True(t, f.Next())
			},
			want: state{current: 1, answers: [][]string{{"eu-west-1"}, nil}},
		},
		{
			name:      "a choice that was not offered is refused",
			questions: []kagenta2a.HITLQuestion{regionQuestion},
			act:       func(t *testing.T, f *AnswerForm) { assert.False(t, f.Choose("mars-1")) },
			want:      state{answers: [][]string{nil}},
		},
		{
			name:      "choose and toggle and set text fit only their own kind",
			questions: []kagenta2a.HITLQuestion{regionQuestion},
			act: func(t *testing.T, f *AnswerForm) {
				assert.False(t, f.Toggle("eu-west-1"))
				assert.False(t, f.SetText("eu-west-1"))
			},
			want: state{answers: [][]string{nil}},
		},
		{
			name:      "toggles keep choice order and untoggle",
			questions: []kagenta2a.HITLQuestion{envQuestion},
			act: func(t *testing.T, f *AnswerForm) {
				f.Toggle("dev")
				f.Toggle("production")
				f.Toggle("staging")
				f.Toggle("production")
			},
			want: state{answers: [][]string{{"staging", "dev"}}, ready: true},
		},
		{
			name:      "untoggling everything unanswers",
			questions: []kagenta2a.HITLQuestion{envQuestion},
			act:       func(t *testing.T, f *AnswerForm) { f.Toggle("dev"); f.Toggle("dev") },
			want:      state{answers: [][]string{{}}},
		},
		{
			name:      "free text is trimmed and blank text is no answer",
			questions: []kagenta2a.HITLQuestion{dbQuestion, regionQuestion},
			act: func(t *testing.T, f *AnswerForm) {
				assert.True(t, f.SetText("   "))
				assert.False(t, f.Next())
				assert.True(t, f.SetText("  PostgreSQL "))
				assert.True(t, f.Next())
			},
			want: state{current: 1, answers: [][]string{{"PostgreSQL"}, nil}},
		},
		{
			name:      "after the last of several questions comes review",
			questions: []kagenta2a.HITLQuestion{regionQuestion, envQuestion},
			act: func(t *testing.T, f *AnswerForm) {
				f.Choose("eu-west-1")
				f.Next()
				f.Toggle("dev")
				assert.True(t, f.Next())
				assert.False(t, f.Next(), "nothing follows review")
			},
			want: state{current: 1, reviewing: true, answers: [][]string{{"eu-west-1"}, {"dev"}}, ready: true},
		},
		{
			name:      "one question is ready without review",
			questions: []kagenta2a.HITLQuestion{regionQuestion},
			act: func(t *testing.T, f *AnswerForm) {
				f.Choose("eu-west-1")
				assert.False(t, f.Next(), "a single question has no review")
			},
			want: state{answers: [][]string{{"eu-west-1"}}, ready: true},
		},
		{
			name:      "several questions are not ready until reviewed",
			questions: []kagenta2a.HITLQuestion{regionQuestion, envQuestion},
			act: func(t *testing.T, f *AnswerForm) {
				f.Choose("eu-west-1")
				f.Next()
				f.Toggle("dev")
			},
			want: state{current: 1, answers: [][]string{{"eu-west-1"}, {"dev"}}},
		},
		{
			name:      "prev from review edits the last question and keeps answers",
			questions: []kagenta2a.HITLQuestion{regionQuestion, envQuestion},
			act: func(t *testing.T, f *AnswerForm) {
				f.Choose("eu-west-1")
				f.Next()
				f.Toggle("dev")
				f.Next()
				assert.True(t, f.Prev())
				assert.True(t, f.Prev())
				assert.False(t, f.Prev(), "nothing precedes the first question")
			},
			want: state{answers: [][]string{{"eu-west-1"}, {"dev"}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pending, _ := askPending(t, tt.questions...)
			form, ok := NewAnswerForm(pending)
			require.True(t, ok)
			tt.act(t, form)

			assert.Equal(t, tt.want.current, form.Current(), "current")
			assert.Equal(t, tt.want.reviewing, form.Reviewing(), "reviewing")
			for i := range form.Questions() {
				if len(tt.want.answers[i]) == 0 {
					assert.Empty(t, form.Selections(i), "answer %d", i)
					assert.False(t, form.Answered(i), "answered %d", i)
					continue
				}
				assert.Equal(t, tt.want.answers[i], form.Selections(i), "answer %d", i)
			}
			assert.Equal(t, tt.want.ready, form.Ready(), "ready")
		})
	}
}

func TestAnswerFormSelectionsAreACopy(t *testing.T) {
	pending, _ := askPending(t, envQuestion)
	form, ok := NewAnswerForm(pending)
	require.True(t, ok)
	form.Toggle("dev")

	form.Selections(0)[0] = "production"

	assert.Equal(t, []string{"dev"}, form.Selections(0))
	assert.True(t, form.Selected(0, "dev"))
	assert.False(t, form.Selected(0, "production"))
}

func TestAnswerFormAnswer(t *testing.T) {
	pending, request := askPending(t, regionQuestion, envQuestion, dbQuestion)
	form, ok := NewAnswerForm(pending)
	require.True(t, ok)
	form.Choose("eu-west-1")
	form.Next()
	form.Toggle("staging")
	form.Toggle("dev")
	form.Next()
	form.SetText("PostgreSQL")
	form.Next()
	require.True(t, form.Ready())

	message, record, err := form.Answer("ctx-1")
	require.NoError(t, err)

	assert.Equal(t, a2atype.TaskID("task-1"), message.TaskID)
	assert.Equal(t, "ctx-1", message.ContextID)
	assert.Equal(t, a2atype.MessageRoleUser, message.Role)
	assert.Equal(t, []string{kagenta2a.HITLExtensionURI}, message.Extensions)
	assert.Equal(t, "eu-west-1\nstaging, dev\nPostgreSQL", message.Parts[0].Text(), "the web UI's fallback prose")

	response, err := kagenta2a.ParseAskUserResponse(message)
	require.NoError(t, err)
	assert.Equal(t, "ask-1", response.ID)
	assert.Equal(t, []kagenta2a.AskUserAnswer{{Answer: []string{"eu-west-1"}}, {Answer: []string{"staging", "dev"}}, {Answer: []string{"PostgreSQL"}}}, response.Answers)
	assert.NoError(t, kagenta2a.ValidateAskUserResponse(request, response), "the runtime accepts it")

	assert.Equal(t, transcript.AnswerRecord{
		Questions: []kagenta2a.HITLQuestion{regionQuestion, envQuestion, dbQuestion},
		Answers:   [][]string{{"eu-west-1"}, {"staging", "dev"}, {"PostgreSQL"}},
	}, record)

	form.Toggle("production")
	assert.Equal(t, [][]string{{"eu-west-1"}, {"staging", "dev"}, {"PostgreSQL"}}, record.Answers, "the record does not change with the form")
}

func TestAnswerFormAnswerNeedsEveryQuestion(t *testing.T) {
	pending, _ := askPending(t, regionQuestion, envQuestion)
	form, ok := NewAnswerForm(pending)
	require.True(t, ok)
	form.Choose("eu-west-1")

	_, _, err := form.Answer("ctx-1")

	assert.Error(t, err, "a gap would answer the wrong question")
}
