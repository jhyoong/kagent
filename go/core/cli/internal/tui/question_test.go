package tui

import (
	"errors"
	"strings"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	tea "github.com/charmbracelet/bubbletea"
	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
	clia2a "github.com/kagent-dev/kagent/go/core/cli/internal/a2a"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	regionQuestion = kagenta2a.HITLQuestion{Question: "Which region?", Choices: []string{"eu-west-1", "us-east-1"}}
	envQuestion    = kagenta2a.HITLQuestion{Question: "Which environments should be migrated?", Choices: []string{"staging", "production", "dev"}, Multiple: true}
	dbQuestion     = kagenta2a.HITLQuestion{Question: "Which database should we use for storage?"}
)

// questionStatus is the input-required status message of an ask_user request with id.
func questionStatus(t *testing.T, id string, questions ...kagenta2a.HITLQuestion) *a2atype.Message {
	t.Helper()
	message := a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart(questions[0].Question))
	require.NoError(t, kagenta2a.AttachHITL(message, kagenta2a.AskUserRequest{
		Type: kagenta2a.HITLTypeAskUserRequest, ID: id, Questions: questions,
	}))
	return message
}

// askedChat returns a chat whose turn parked asking questions.
func askedChat(t *testing.T, questions ...kagenta2a.HITLQuestion) (*chatModel, *fakeTurnClient) {
	t.Helper()
	m, client := streamingChat(t, true)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	pause(m, questionStatus(t, "ask-1", questions...))
	_, awaiting := m.turn.(*awaitingTurn)
	require.True(t, awaiting, "the turn waits for an answer")
	return m, client
}

func typeText(m *chatModel, text string) {
	for _, r := range text {
		press(m, string(r))
	}
}

func sentAnswers(t *testing.T, client *fakeTurnClient) [][]string {
	t.Helper()
	require.Len(t, client.sent, 2, "the answer was sent")
	response, err := kagenta2a.ParseAskUserResponse(client.sent[1].Message)
	require.NoError(t, err)
	answers := make([][]string, 0, len(response.Answers))
	for _, answer := range response.Answers {
		answers = append(answers, answer.Answer)
	}
	return answers
}

func TestChatModelAnswersQuestionsInOrder(t *testing.T) {
	m, client := askedChat(t, regionQuestion, envQuestion, dbQuestion)

	view := m.bottomView(m.vp.Width)
	assert.Contains(t, view, "? The agent asked you something")
	assert.Contains(t, view, "(1 of 3)")
	assert.Contains(t, view, "Which region?")
	assert.Contains(t, view, "choose one")
	assert.NotContains(t, view, "ctrl+p previous", "nothing precedes the first question")

	press(m, "down", "enter")
	view = m.bottomView(m.vp.Width)
	assert.Contains(t, view, "(2 of 3)")
	assert.Contains(t, view, "Which environments should be migrated?")
	assert.Contains(t, view, "choose any")
	assert.Contains(t, view, "↑↓ move · space toggle · enter next · ctrl+p previous · ctrl+x discard")

	press(m, "enter")
	assert.Contains(t, m.bottomView(m.vp.Width), "(2 of 3)", "a multi-select needs one choice")
	press(m, "space", "down", "down", "space")
	view = m.bottomView(m.vp.Width)
	assert.Contains(t, view, "[x] staging")
	assert.Contains(t, view, "[ ] production")
	assert.Contains(t, view, "> [x] dev")

	press(m, "enter")
	assert.Contains(t, m.bottomView(m.vp.Width), "(3 of 3)")
	typeText(m, "PostgreSQL")
	assert.Contains(t, m.bottomView(m.vp.Width), "› PostgreSQL")
	press(m, "enter")

	view = m.bottomView(m.vp.Width)
	assert.Contains(t, view, "? Review answers")
	assert.Contains(t, view, "1 Which region?")
	assert.Contains(t, view, "us-east-1")
	assert.Contains(t, view, "staging, dev")
	assert.Contains(t, view, "enter send · ctrl+p edit · ctrl+x discard")
	require.Len(t, client.sent, 1, "review waits for enter")

	press(m, "enter")

	assert.Equal(t, [][]string{{"us-east-1"}, {"staging", "dev"}, {"PostgreSQL"}}, sentAnswers(t, client))
	sent := client.sent[1].Message
	assert.Equal(t, a2atype.TaskID("task-1"), sent.TaskID, "the answer resumes the paused task")
	assert.Equal(t, "ctx-1", sent.ContextID)
	assert.Contains(t, sent.Extensions, kagenta2a.HITLExtensionURI)

	assert.True(t, m.isStreaming())
	shown := shownText(m)
	assert.Contains(t, shown, "? Which region? → us-east-1")
	assert.Contains(t, shown, "? Which environments should be migrated? → staging, dev")
	assert.NotContains(t, shown, "You: us-east-1", "the record is shown, not the fallback text")
	assert.Contains(t, m.View(), "Type a message", "the composer is back")
}

func TestChatModelCtrlPGoesBackKeepingAnswers(t *testing.T) {
	m, client := askedChat(t, regionQuestion, dbQuestion)

	press(m, "down", "enter")
	typeText(m, "MySQL")
	press(m, "ctrl+p")
	view := m.bottomView(m.vp.Width)
	assert.Contains(t, view, "(1 of 2)")
	assert.Contains(t, view, "> (•) us-east-1", "the cursor starts on the earlier choice")

	press(m, "up", "enter")
	assert.Contains(t, m.bottomView(m.vp.Width), "› MySQL", "typed text survives going back")
	press(m, "esc")
	assert.Contains(t, m.bottomView(m.vp.Width), "(1 of 2)", "esc in free text goes back too")
	press(m, "enter")
	typeText(m, "?")
	press(m, "enter")
	assert.Contains(t, m.bottomView(m.vp.Width), "MySQL?")

	press(m, "ctrl+p")
	assert.Contains(t, m.bottomView(m.vp.Width), "› MySQL?", "review edits the last question")
	press(m, "enter", "enter")

	assert.Equal(t, [][]string{{"eu-west-1"}, {"MySQL?"}}, sentAnswers(t, client))
}

func TestChatModelSingleFreeTextQuestionSendsOnEnter(t *testing.T) {
	m, client := askedChat(t, dbQuestion)

	view := m.bottomView(m.vp.Width)
	assert.Contains(t, view, "? Which database should we use for storage?")
	assert.NotContains(t, view, "of 1")
	assert.Contains(t, view, "enter send · ctrl+x discard")

	press(m, "enter", "esc")
	require.Len(t, client.sent, 1, "a blank answer is no answer")
	_, awaiting := m.turn.(*awaitingTurn)
	require.True(t, awaiting, "esc on the only question does not leave it")

	typeText(m, "PostgreSQL")
	press(m, "enter")

	assert.Equal(t, [][]string{{"PostgreSQL"}}, sentAnswers(t, client))
	assert.Equal(t, "PostgreSQL", client.sent[1].Message.Parts[0].Text(), "the web UI's fallback prose")
	assert.Contains(t, shownText(m), "? Which database should we use for storage? → PostgreSQL")
}

func TestChatModelSingleChoiceSendsOnEnter(t *testing.T) {
	m, client := askedChat(t, regionQuestion)

	assert.Contains(t, m.bottomView(m.vp.Width), "↑↓ move · enter send · ctrl+x discard")
	press(m, "enter")

	assert.Equal(t, [][]string{{"eu-west-1"}}, sentAnswers(t, client))
}

func TestChatModelQuestionDiscard(t *testing.T) {
	m, client := askedChat(t, dbQuestion)
	typeText(m, "y")

	press(m, "ctrl+x")
	runCmd(press(m, "y"))

	assert.Equal(t, []a2atype.TaskID{"task-1"}, client.cancels, "ctrl+x reaches the chat while the text field is open")
}

// After an answer the resumed stream may pause again at once, with no other
// state in between; that is a new request, while a replay of the answered one is not.
func TestChatModelResumedStreamThatPausesAgainPrompts(t *testing.T) {
	m, client := askedChat(t, regionQuestion)
	answered := questionStatus(t, "ask-1", regionQuestion)
	answered.ID = m.turn.(*awaitingTurn).paused.Status.Message.ID
	press(m, "enter")
	require.True(t, m.isStreaming())

	deliver(m, clia2a.StreamResult{Event: &a2atype.Task{
		ID: "task-1", ContextID: "ctx-1",
		Status: a2atype.TaskStatus{State: a2atype.TaskStateInputRequired, Message: answered},
	}})
	require.True(t, m.isStreaming(), "a replay of the answered pause is not a new pause")

	pause(m, questionStatus(t, "ask-2", dbQuestion))

	turn, awaiting := m.turn.(*awaitingTurn)
	require.True(t, awaiting, "the second pause prompts")
	assert.Contains(t, m.bottomView(m.vp.Width), "Which database should we use for storage?")
	typeText(m, "PostgreSQL")
	press(m, "enter")
	require.Len(t, client.sent, 3)
	response, err := kagenta2a.ParseAskUserResponse(client.sent[2].Message)
	require.NoError(t, err)
	assert.Equal(t, "ask-2", response.ID)
	assert.Equal(t, a2atype.TaskID("task-1"), turn.pending.TaskID)
}

// A resume whose stream fails leaves the server holding the request, so the
// chat must not go idle with no way to answer or discard it.
func TestChatModelFailedResumeRestoresThePrompt(t *testing.T) {
	waiting := &a2atype.Task{ID: "task-1", ContextID: "ctx-1", Status: a2atype.TaskStatus{State: a2atype.TaskStateInputRequired}}
	tests := []struct {
		name     string
		task     *a2atype.Task
		err      error
		awaiting bool
		banner   string
	}{
		{name: "the task still waits", task: waiting, awaiting: true, banner: "still waiting for input"},
		{name: "the task cannot be read", err: errors.New("unavailable"), awaiting: true, banner: "unavailable"},
		{
			name:   "the answer got through",
			task:   &a2atype.Task{ID: "task-1", ContextID: "ctx-1", Status: a2atype.TaskStatus{State: a2atype.TaskStateWorking}},
			banner: "no longer waiting for input",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, client := askedChat(t, regionQuestion)
			if tt.task != nil {
				task := *tt.task
				task.Status.Message = m.turn.(*awaitingTurn).paused.Status.Message
				client.getResult = &task
			}
			client.getErr = tt.err
			press(m, "enter")

			for _, msg := range runCmd(deliver(m, clia2a.StreamResult{Err: errors.New("connection reset")})) {
				m.Update(msg)
			}

			assert.Equal(t, []a2atype.TaskID{"task-1"}, client.gets, "the chat asks where the task stands")
			assert.Contains(t, shownText(m), tt.banner)
			_, awaiting := m.turn.(*awaitingTurn)
			require.Equal(t, tt.awaiting, awaiting)
			if !tt.awaiting {
				return
			}
			press(m, "enter")
			require.Len(t, client.sent, 3, "the request can be answered again")
			assert.Equal(t, a2atype.TaskID("task-1"), client.sent[2].Message.TaskID)
		})
	}
}

func TestChatModelFailedResumeCheckYieldsToANewTurn(t *testing.T) {
	m, client := askedChat(t, regionQuestion)
	client.getResult = &a2atype.Task{ID: "task-1", Status: a2atype.TaskStatus{State: a2atype.TaskStateInputRequired, Message: questionStatus(t, "ask-1", regionQuestion)}}
	press(m, "enter")
	cmd := deliver(m, clia2a.StreamResult{Err: errors.New("connection reset")})
	m.submit("something else")

	for _, msg := range runCmd(cmd) {
		m.Update(msg)
	}

	assert.True(t, m.isStreaming(), "the turn the user started wins")
	assert.False(t, strings.Contains(m.bottomView(m.vp.Width), "Which region?"))
}
