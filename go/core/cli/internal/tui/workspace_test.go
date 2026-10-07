package tui

import (
	"slices"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"trpc.group/trpc-go/trpc-a2a-go/protocol"

	api "github.com/kagent-dev/kagent/go/api/httpapi"
	"github.com/kagent-dev/kagent/go/core/cli/internal/config"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/theme"
)

// newTestWorkspace is a workspace with an agent and an open session, sized for rendering.
func newTestWorkspace(t *testing.T) *workspaceModel {
	t.Helper()
	cfg := &config.Config{KAgentURL: "http://127.0.0.1:0", Timeout: time.Second}
	m := newWorkspaceModel(cfg, cfg.Client(), false)
	m.agent = &api.AgentResponse{ID: "shop/billing"}
	m.agentRef = "shop/billing"
	session := &api.Session{ID: "sess-1"}
	m.sessions.SetItems([]list.Item{sessionListItem{s: session}, sessionListItem{s: &api.Session{ID: "sess-2"}}})
	m.current = session
	m.focus = focusChat
	m.startChat(false)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return m
}

func wsPress(m *workspaceModel, k string) tea.Cmd {
	_, cmd := m.Update(keyMsg(k))
	return cmd
}

// quits reports whether cmd, run, asks the program to quit.
func quits(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	switch msg := cmd().(type) {
	case tea.QuitMsg:
		return true
	case tea.BatchMsg:
		return slices.ContainsFunc(msg, quits)
	}
	return false
}

func TestWorkspaceKeysReachOnlyTheFocusedPane(t *testing.T) {
	m := newTestWorkspace(t)

	for _, k := range []string{"q", "j"} {
		wsPress(m, k)
	}
	assert.Equal(t, "qj", m.chat.input.Value(), "chat focus: keys are composer text")
	assert.Equal(t, 0, m.sessions.Index(), "the session list did not move")

	wsPress(m, "tab")
	require.Equal(t, focusSessions, m.focus)
	assert.False(t, m.chat.input.Focused(), "the composer gives up the cursor")
	wsPress(m, "j")
	assert.Equal(t, 1, m.sessions.Index(), "sessions focus: keys move the list")
	assert.Equal(t, "qj", m.chat.input.Value(), "and do not reach the composer")

	assert.False(t, quits(wsPress(m, "q")), "q does not quit from the session list")
	assert.False(t, quits(wsPress(m, "esc")), "esc does not quit from the session list")
}

func TestWorkspaceShowsFocus(t *testing.T) {
	m := newTestWorkspace(t)
	m.View()
	assert.Contains(t, ansi.Strip(m.View()), "━━━", "the chat is marked as focused")
	assert.NotEqual(t, theme.ColorPrimary, m.sessions.Styles.Title.GetBackground())

	wsPress(m, "tab")
	view := ansi.Strip(m.View())
	assert.NotContains(t, view, "━━━", "the chat no longer is")
	assert.Equal(t, theme.ColorPrimary, m.sessions.Styles.Title.GetBackground(), "the session list title is highlighted")
}

func TestFooterAdvertisesPageKeys(t *testing.T) {
	m := newTestWorkspace(t)
	assert.Contains(t, ansi.Strip(m.View()), "pgup/pgdn")
}

func TestWorkspaceNamingKeepsKeysFromChat(t *testing.T) {
	m := newTestWorkspace(t)
	wsPress(m, "ctrl+s")
	require.True(t, m.naming)
	for _, k := range []string{"n", "e", "w"} {
		wsPress(m, k)
	}
	assert.Equal(t, "new", m.sessionInput.Value())
	assert.Empty(t, m.chat.input.Value())
}

func TestWorkspaceSelectLayout(t *testing.T) {
	m := newTestWorkspace(t)
	m.showDetails = true
	m.resize()
	assert.Contains(t, ansi.Strip(m.View()), "Sessions")

	wsPress(m, "ctrl+l")
	assert.True(t, m.selectLayout)
	view := ansi.Strip(m.View())
	assert.NotContains(t, view, "Sessions", "the sidebar is hidden")
	assert.NotContains(t, view, "Agent: ", "the details pane is hidden")
	assert.Equal(t, 120, m.chat.vp.Width, "the chat takes the full width")

	assert.Nil(t, wsPress(m, "tab"), "tab is disabled in the select layout")
	assert.Equal(t, focusChat, m.focus)

	wsPress(m, "ctrl+l")
	assert.False(t, m.selectLayout)
	assert.Contains(t, ansi.Strip(m.View()), "Sessions")
}

func TestWorkspaceHistoryGoesToItsSession(t *testing.T) {
	m := newTestWorkspace(t)
	paused := &protocol.Task{ID: "task-1", Status: protocol.TaskStatus{
		State: protocol.TaskStateInputRequired, Message: twoToolsPaused(t).Status.Message,
	}}

	m.Update(sessionHistoryLoadedMsg{sessionID: "other", items: []*protocol.Task{paused}})
	assert.IsType(t, idleTurn{}, m.chat.turn, "history of another session is dropped")

	m.Update(sessionHistoryLoadedMsg{sessionID: "sess-1", items: []*protocol.Task{paused}})
	assert.IsType(t, &awaitingTurn{}, m.chat.turn, "the pending request is restored")
}

func TestWorkspaceCtrlOFoldsFromAnyPane(t *testing.T) {
	m := newTestWorkspace(t)
	wsPress(m, "tab")
	wsPress(m, "ctrl+o")
	assert.True(t, m.chat.folds.Expanded(0))
}
