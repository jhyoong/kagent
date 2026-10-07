package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	tea "github.com/charmbracelet/bubbletea"
	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	clia2a "github.com/kagent-dev/kagent/go/core/cli/internal/a2a"
	"github.com/kagent-dev/kagent/go/core/cli/internal/connection"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/transcript"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var testTime = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// fakeLister serves one canned page per call, so paging is observable.
type fakeLister struct {
	pages    []*apiv1alpha1.ListSessionsResponse
	err      error
	requests []*apiv1alpha1.ListSessionsRequest
}

func (f *fakeLister) ListSessions(_ context.Context, request *apiv1alpha1.ListSessionsRequest) (*apiv1alpha1.ListSessionsResponse, error) {
	f.requests = append(f.requests, request)
	if f.err != nil {
		return nil, f.err
	}
	return f.pages[min(len(f.requests)-1, len(f.pages)-1)], nil
}

// fakeCatalog stands in for the Kubernetes read.
type fakeCatalog struct {
	namespaces []namespaceCount
	agents     []string
	err        error
}

func (f *fakeCatalog) Namespaces(context.Context) ([]namespaceCount, error) {
	return f.namespaces, f.err
}
func (f *fakeCatalog) Agents(context.Context, string) ([]string, error) {
	return f.agents, f.err
}

func testWorkspace(t *testing.T, lister sessionLister) *workspaceModel {
	t.Helper()
	conn := connection.DefaultOptions()
	conn.Namespace = "kagent"
	conn.Timeout = 30 * time.Second
	api, err := conn.APIClient()
	require.NoError(t, err)
	t.Cleanup(func() { _ = api.Close() })
	gateway, err := conn.GatewayClient()
	require.NoError(t, err)
	t.Cleanup(func() { _ = gateway.Close() })

	m := newWorkspaceModel(t.Context(), Options{Namespace: conn.Namespace}, api, gateway, nil, nil, false)
	m.lister = lister
	m.width, m.height = 120, 40
	return m
}

func workspaceSession(id, template string, state apiv1alpha1.RuntimeState, created time.Time) *apiv1alpha1.Session {
	return &apiv1alpha1.Session{
		Id: id,

		Agent: &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: template},

		State:     state,
		CreatedAt: timestamppb.New(created),
	}
}

func readySession(id, template string) *apiv1alpha1.Session {
	return workspaceSession(id, template, apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, testTime)
}

func page(nextToken string, sessions ...*apiv1alpha1.Session) *apiv1alpha1.ListSessionsResponse {
	return &apiv1alpha1.ListSessionsResponse{
		Sessions: sessions,
		Page:     &apiv1alpha1.PageResponse{NextPageToken: nextToken},
	}
}

// loaded runs the session fetch and applies it, returning the follow-up command.
func loaded(m *workspaceModel) tea.Cmd {
	return m.applySessions(m.loadSessions()().(sessionsLoadedMsg))
}

// runBatch executes a command's messages, flattening one level of batching.
func runBatch(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if batch, ok := cmd().(tea.BatchMsg); ok {
		for _, batched := range batch {
			batched()
		}
	}
}

func TestWorkspaceLoadsSessions(t *testing.T) {
	one := readySession("a", "smoke")
	tests := []struct {
		name          string
		lister        *fakeLister
		wantSessions  int
		wantRequests  int
		wantTruncated bool
		wantStatus    string
	}{
		{
			name: "follows the next page token",
			lister: &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{
				page("token-2", one), page("", readySession("b", "reporter")),
			}},
			wantSessions: 2, wantRequests: 2,
		},
		{
			name:         "stops at the page bound rather than looping",
			lister:       &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{page("more", one)}},
			wantSessions: maxSessionPages, wantRequests: maxSessionPages,
			wantTruncated: true, wantStatus: "more pages are available",
		},
		{
			name:       "reports a failure",
			lister:     &fakeLister{err: errors.New("unavailable")},
			wantStatus: "Failed to load Sessions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testWorkspace(t, tt.lister)
			msg := m.loadSessions()().(sessionsLoadedMsg)
			m.applySessions(msg)

			assert.Len(t, msg.sessions, tt.wantSessions)
			assert.Equal(t, tt.wantTruncated, msg.truncated)
			if tt.wantRequests > 0 {
				assert.Len(t, tt.lister.requests, tt.wantRequests)
			}
			if tt.wantStatus == "" {
				assert.Empty(t, m.status)
			} else {
				assert.Contains(t, m.status, tt.wantStatus)
			}
		})
	}
}

func TestWorkspaceSortsNewestFirstAndOpensOne(t *testing.T) {
	older := workspaceSession("old", "a", apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, testTime.Add(-time.Hour))
	newer := readySession("new", "b")
	m := testWorkspace(t, &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{page("", older, newer)}})

	cmd := loaded(m)

	require.Len(t, m.all, 2)
	assert.Equal(t, "new", m.all[0].GetId(), "newest sorts first")
	require.NotNil(t, cmd, "the first session opens automatically")
	assert.Equal(t, newer, cmd().(sessionSelectedMsg).session)
}

func TestWorkspaceSelectSession(t *testing.T) {
	tests := []struct {
		name       string
		state      apiv1alpha1.RuntimeState
		wantChat   bool
		wantStatus string
	}{
		{name: "ready opens a chat", state: apiv1alpha1.RuntimeState_RUNTIME_STATE_READY, wantChat: true},
		// The gateway rejects every call for a non-READY session, so the workspace must not dial one.
		{name: "suspended is not dialed", state: apiv1alpha1.RuntimeState_RUNTIME_STATE_SUSPENDED, wantStatus: "SUSPENDED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := workspaceSession("44444444-4444-4444-4444-444444444444", "reporter", tt.state, testTime)
			m := testWorkspace(t, &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{page("", session)}})
			loaded(m)

			cmd := m.selectSession(session)

			if tt.wantChat {
				require.NotNil(t, m.chat)
				assert.Equal(t, session.GetId(), m.chat.contextID)
				assert.NotNil(t, cmd, "history loads for a READY session")
				return
			}
			assert.Nil(t, m.chat)
			assert.Nil(t, cmd, "a non-READY session loads no history")
			assert.Contains(t, m.status, tt.wantStatus)
			assert.Contains(t, m.View(), tt.wantStatus)
		})
	}
}

// Moving the cursor filters immediately; enter only drills down.
func TestWorkspaceCascadeFiltersOnCursorMove(t *testing.T) {
	m := testWorkspace(t, &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{
		page("", readySession("a", "smoke"), readySession("c", "reporter")),
	}})
	loaded(m)

	// Every distinct template gets a row, after "(all)".
	require.Len(t, m.agents.Items(), 3)
	assert.Equal(t, nameItem{name: allNames, count: 2}, m.agents.Items()[0])
	require.Len(t, m.sessions.Items(), 2)

	m.focus = panelAgents
	m.forward(tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, "reporter", m.agent)
	require.Len(t, m.sessions.Items(), 1)
	assert.Equal(t, "c", m.sessions.Items()[0].(sessionItem).GetId())

	m.forward(tea.KeyMsg{Type: tea.KeyUp}) // back to "(all)"
	assert.Empty(t, m.agent)
	assert.Len(t, m.sessions.Items(), 2)
}

func TestWorkspaceKeys(t *testing.T) {
	tests := []struct {
		name       string
		focus      panelID
		key        tea.KeyMsg
		wantFocus  panelID
		wantReload bool
	}{
		{
			name: "a digit focuses its panel", focus: panelSessions,
			key:       tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")},
			wantFocus: panelAgents,
		},
		{
			name: "tab cycles forward", focus: panelSessions,
			key: tea.KeyMsg{Type: tea.KeyTab}, wantFocus: panelChat,
		},
		{
			name: "enter drills down a cascade panel", focus: panelNamespaces,
			key: tea.KeyMsg{Type: tea.KeyEnter}, wantFocus: panelAgents,
		},
		{
			name: "ctrl+r reloads", focus: panelChat,
			key: tea.KeyMsg{Type: tea.KeyCtrlR}, wantFocus: panelChat, wantReload: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testWorkspace(t, &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{page("")}})
			m.focus = tt.focus

			cmd, handled := m.handleKey(tt.key)

			require.True(t, handled, "the key must be consumed")
			assert.Equal(t, tt.wantFocus, m.focus)
			if tt.wantReload {
				require.NotNil(t, cmd)
				assert.IsType(t, sessionsLoadedMsg{}, cmd())
			}
		})
	}
}

func TestWorkspaceMouse(t *testing.T) {
	// Offsets are from the session panel's top border: +2 is its first row.
	tests := []struct {
		name        string
		x, rowBelow int
		action      tea.MouseAction
		wantHandled bool
		wantFocus   panelID
		wantIndex   int
	}{
		{
			name: "a row click focuses the panel and selects that row",
			x:    4, rowBelow: 3, action: tea.MouseActionRelease,
			wantHandled: true, wantFocus: panelSessions, wantIndex: 1,
		},
		{
			name: "a chat click focuses the chat",
			x:    sidebarWidth + 5, rowBelow: 1, action: tea.MouseActionRelease,
			wantHandled: true, wantFocus: panelChat,
		},
		{
			name: "motion is not a click",
			x:    4, rowBelow: 3, action: tea.MouseActionMotion,
			wantFocus: panelSessions,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testWorkspace(t, &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{
				page("", readySession("a", "smoke"), readySession("b", "reporter")),
			}})
			loaded(m)
			m.resize()
			m.focus = panelSessions
			_, top := m.panelListAt(panelSessions)

			_, handled := m.handleMouse(tea.MouseMsg{
				X: tt.x, Y: top + tt.rowBelow, Action: tt.action, Button: tea.MouseButtonLeft,
			})

			assert.Equal(t, tt.wantHandled, handled)
			assert.Equal(t, tt.wantFocus, m.focus)
			assert.Equal(t, tt.wantIndex, m.sessions.Index())
		})
	}
}

func TestWorkspaceCatalog(t *testing.T) {
	tests := []struct {
		name       string
		catalog    catalog
		wantNames  map[string]int
		wantStatus string
	}{
		{
			name: "an unused template is listed at zero",
			catalog: &fakeCatalog{
				agents: []string{"smoke", "reporter"},
			},
			wantNames: map[string]int{allNames: 1, "smoke": 1, "reporter": 0},
		},
		{
			// Without a kubeconfig the cascade still works, from session data.
			name:       "falls back to session names",
			catalog:    &fakeCatalog{err: errors.New("no kubeconfig")},
			wantNames:  map[string]int{allNames: 1, "smoke": 1},
			wantStatus: "only Agents that have sessions",
		},
		{
			name:       "a nil catalog is not fatal",
			catalog:    nil,
			wantNames:  map[string]int{allNames: 1, "smoke": 1},
			wantStatus: "no Kubernetes client",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testWorkspace(t, &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{
				page("", readySession("a", "smoke")),
			}})
			m.catalog = tt.catalog
			loaded(m)

			m.Update(m.loadCatalog()())

			got := map[string]int{}
			for _, item := range m.agents.Items() {
				row := item.(nameItem)
				got[row.name] = row.count
			}
			assert.Equal(t, tt.wantNames, got)
			if tt.wantStatus != "" {
				assert.Contains(t, m.status, tt.wantStatus)
			}
		})
	}
}

func TestWorkspaceNamespacePanel(t *testing.T) {
	tests := []struct {
		name       string
		catalog    *fakeCatalog
		want       []nameItem
		wantStatus string
	}{
		{
			name: "lists namespaces with template counts",
			catalog: &fakeCatalog{namespaces: []namespaceCount{
				{Name: "kagent", Agents: 3}, {Name: "team-b", Agents: 1},
			}},
			want: []nameItem{{name: "kagent", count: 3}, {name: "team-b", count: 1}},
		},
		{
			// The panel must show where the user is even when the cluster-wide list was refused.
			name:       "always lists the current namespace",
			catalog:    &fakeCatalog{err: errors.New("forbidden")},
			want:       []nameItem{{name: "kagent"}},
			wantStatus: "only the current namespace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testWorkspace(t, &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{page("")}})
			m.catalog = tt.catalog

			m.Update(m.loadNamespaces()())

			got := make([]nameItem, 0, len(m.namespaces.Items()))
			for _, item := range m.namespaces.Items() {
				got = append(got, item.(nameItem))
			}
			assert.Equal(t, tt.want, got)
			assert.Equal(t, 0, m.namespaces.Index(), "the current namespace starts selected")
			if tt.wantStatus != "" {
				assert.Contains(t, m.status, tt.wantStatus)
			}
		})
	}
}

func TestWorkspaceSwitchingNamespaceRefetches(t *testing.T) {
	lister := &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{
		page("", readySession("a", "smoke")),
	}}
	m := testWorkspace(t, lister)
	m.catalog = &fakeCatalog{namespaces: []namespaceCount{{Name: "kagent"}, {Name: "team-b"}}}
	m.Update(m.loadNamespaces()())
	loaded(m)
	require.Len(t, m.all, 1)

	m.focus = panelNamespaces
	cmd := m.forward(tea.KeyMsg{Type: tea.KeyDown})

	assert.Equal(t, "team-b", m.namespace)
	assert.Empty(t, m.all, "the previous namespace's sessions are cleared")
	assert.Nil(t, m.chat, "the open chat belonged to the old namespace")
	// The port-forward was established before the TUI started, so switching must not disturb it.
	assert.Equal(t, "kagent", m.cfg.Namespace, "the connection's namespace is untouched")

	runBatch(cmd)
	assert.NotEmpty(t, lister.requests)
}

// The delegate renders nothing it cannot type-assert, so these assert on output not model state.
func TestWorkspaceRenders(t *testing.T) {
	const id = "66666666-6666-6666-6666-666666666666"
	tests := []struct {
		name     string
		sessions []*apiv1alpha1.Session
		render   func(*workspaceModel) string
		want     []string
	}{
		{
			name:     "rows carry template, short ID, and a state glyph",
			sessions: []*apiv1alpha1.Session{readySession(id, "smoke")},
			render:   func(m *workspaceModel) string { return m.sessions.View() },
			want:     []string{"smoke", "66666666", "●"},
		},
		{
			name:   "an empty namespace says how to create a session",
			render: func(m *workspaceModel) string { return m.View() },
			want:   []string{"No Sessions", "kagent agent session create --agent A"},
		},
		{
			name:     "details keep the full copyable ID",
			sessions: []*apiv1alpha1.Session{readySession(id, "reporter")},
			render: func(m *workspaceModel) string {
				m.current = m.all[0]
				m.renderDetails()
				return m.details
			},
			want: []string{id, "reporter", "READY"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testWorkspace(t, &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{page("", tt.sessions...)}})
			loaded(m)
			m.resize()

			got := tt.render(m)
			for _, want := range tt.want {
				assert.Contains(t, got, want)
			}
		})
	}
}

// Async replies name their session, so a late one must not land in the new chat.
func TestWorkspaceIgnoresHistoryForAnotherSession(t *testing.T) {
	first, second := readySession("a", "smoke"), readySession("b", "reporter")
	m := testWorkspace(t, &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{page("", first, second)}})
	loaded(m)
	m.selectSession(second)
	before := shownText(m.chat)

	m.Update(sessionHistoryLoadedMsg{
		sessionID: first.GetId(),
		tasks:     []*a2atype.Task{{ID: "t", Status: a2atype.TaskStatus{State: a2atype.TaskStateCompleted}}},
	})

	assert.Equal(t, before, shownText(m.chat), "history for another session is dropped")
}

func TestWorkspaceStopsTheOutgoingStreamOnSwitch(t *testing.T) {
	first, second := readySession("a", "smoke"), readySession("b", "reporter")
	m := testWorkspace(t, &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{page("", first, second)}})
	loaded(m)
	m.selectSession(first)
	stopped := false
	m.chat.turn = &streamingTurn{stop: func() { stopped = true }}

	m.selectSession(second)

	assert.True(t, stopped, "the previous session's stream is cancelled")
}

// A chat streams under the workspace's context, so cancelling the program cancels the request.
func TestChatStreamsUnderTheWorkspaceContext(t *testing.T) {
	ready := readySession("a", "smoke")
	ctx, cancel := context.WithCancel(context.Background())
	m := testWorkspace(t, &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{page("", ready)}})
	m.ctx = ctx
	loaded(m)
	m.selectSession(ready)

	client := &fakeTurnClient{}
	m.chat.client = client
	m.chat.submit("hello")

	sent := client.streamCtx
	require.NotNil(t, sent, "the chat never started a stream")

	cancel()
	select {
	case <-sent.Done():
		assert.ErrorIs(t, sent.Err(), context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancelling the workspace did not cancel the stream")
	}
}

// Stream messages must reach the chat even when a panel has focus, or the reply is stranded.
func TestWorkspaceRoutesStreamMessagesRegardlessOfFocus(t *testing.T) {
	ready := readySession("a", "smoke")
	m := testWorkspace(t, &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{page("", ready)}})
	loaded(m)
	m.selectSession(ready)
	m.chat.client = &fakeTurnClient{}
	m.chat.submit("hi")
	m.focus = panelSessions

	m.Update(streamMsg{gen: m.chat.streamGen, result: clia2a.StreamResult{Err: errors.New("stream disconnected")}})

	assert.Contains(t, shownText(m.chat), "Connection error")
}

func TestWorkspaceRefreshDropsADeletedSession(t *testing.T) {
	ready := readySession("a", "smoke")
	lister := &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{page("", ready), page("")}}
	m := testWorkspace(t, lister)
	loaded(m)
	m.selectSession(ready)
	require.NotNil(t, m.chat)

	loaded(m) // second page is empty: the session is gone

	assert.Nil(t, m.chat, "a deleted session leaves no chat behind")
	assert.Nil(t, m.current)
	assert.Contains(t, m.status, "no longer exists")
}

// openedChat returns a workspace whose chat has focus and a scrollable transcript.
func openedChat(t *testing.T) *workspaceModel {
	t.Helper()
	ready := readySession("a", "smoke")
	m := testWorkspace(t, &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{page("", ready)}})
	loaded(m)
	m.selectSession(ready)
	m.focus = panelChat
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.chat.vp.SetContent(strings.Repeat("line\n", 200))
	m.chat.vp.GotoTop()
	return m
}

// Typing must reach the composer: panel digits and viewport letters are not shortcuts there.
func TestWorkspaceComposerKeepsTypedKeys(t *testing.T) {
	for _, typed := range []string{"0", "1", "2", "3", "4", "k", "j", "b", "f", "u", "d", "h", "l", " ", "/"} {
		t.Run(typed, func(t *testing.T) {
			m := openedChat(t)
			offset := m.chat.vp.YOffset

			key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(typed)}
			if typed == " " {
				key = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(typed)}
			}
			m.Update(key)

			assert.Equal(t, typed, m.chat.input.Value())
			assert.Equal(t, panelChat, m.focus)
			assert.Equal(t, offset, m.chat.vp.YOffset)
		})
	}
}

func TestWorkspaceComposerEscDoesNotQuit(t *testing.T) {
	m := openedChat(t)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if cmd != nil {
		assert.NotEqual(t, tea.Quit(), cmd(), "esc in the composer must not quit")
	}
	assert.Equal(t, panelChat, m.focus)
}

func TestWorkspaceComposerPageKeysStillScroll(t *testing.T) {
	m := openedChat(t)

	m.Update(tea.KeyMsg{Type: tea.KeyPgDown})

	assert.Positive(t, m.chat.vp.YOffset)
}

func toolEntry(id string) transcript.ToolActivity {
	return transcript.ToolActivity{
		ID: id, Name: "get_logs", Args: map[string]any{"pod": "checkout"},
		Outcome: transcript.Returned{Response: map[string]any{"result": "panic: missing env"}},
	}
}

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestWorkspaceCtrlOTogglesAllToolOutput(t *testing.T) {
	m := openedChat(t)
	m.chat.appendEntry(toolEntry("a"))

	assert.NotContains(t, m.chat.vp.View(), "panic: missing env")

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	assert.Contains(t, m.chat.vp.View(), "▾ ✓ get_logs")
	assert.Contains(t, m.chat.vp.View(), "panic: missing env")

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	assert.Contains(t, m.chat.vp.View(), "▸ ✓ get_logs")
	assert.NotContains(t, m.chat.vp.View(), "panic: missing env")
}

func TestWorkspaceSelectModeFoldsOneEntryAndReturnsToComposer(t *testing.T) {
	m := openedChat(t)
	m.chat.appendEntry(toolEntry("a"))
	m.chat.appendEntry(toolEntry("b"))

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	require.Equal(t, modeSelect, m.chat.mode)
	assert.Equal(t, 1, m.chat.selected, "starts on the newest entry")
	assert.Contains(t, m.footerView(), "fold: enter, space")

	m.Update(runes("k"))
	assert.Equal(t, 0, m.chat.selected)
	assert.Empty(t, m.chat.input.Value(), "select-mode keys are not typed")

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, m.chat.folds.Expanded(0))
	assert.False(t, m.chat.folds.Expanded(1))
	assert.Equal(t, 1, strings.Count(m.chat.vp.View(), "panic: missing env"))

	m.Update(runes("o"))
	assert.Equal(t, 2, strings.Count(m.chat.vp.View(), "panic: missing env"), "o flips the default over the single toggle")

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	assert.Equal(t, modeCompose, m.chat.mode)
	assert.NotContains(t, m.footerView(), "fold: enter, space")

	m.Update(runes("j"))
	assert.Equal(t, "j", m.chat.input.Value(), "typing works again")
}

func TestWorkspaceCtrlGWithoutEntriesStaysInComposer(t *testing.T) {
	m := openedChat(t)

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})

	assert.Equal(t, modeCompose, m.chat.mode)
}

// esc esc in the composer cancels through the workspace; the result reaches the chat wherever focus is.
func TestWorkspaceDoubleEscCancelsTheRunningTurn(t *testing.T) {
	m := openedChat(t)
	client := &fakeTurnClient{cancelErr: errors.New("boom")}
	m.chat.client = client
	m.chat.submit("hi")
	m.Update(streamMsg{gen: m.chat.streamGen, result: clia2a.StreamResult{Event: a2atype.NewStatusUpdateEvent(reqCtx(), a2atype.TaskStateWorking, nil)}})
	assert.Contains(t, m.footerView(), "cancel: esc esc")

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	msgs := runCmd(cmd)
	require.Len(t, msgs, 1)
	assert.NotEqual(t, tea.Quit(), msgs[0])
	assert.Equal(t, []a2atype.TaskID{"task-1"}, client.cancels)

	m.focus = panelSessions
	m.Update(msgs[0])
	assert.Contains(t, shownText(m.chat), "boom", "a cancel failure is shown")
}

// Keys the workspace consumes still count as "another key" and disarm the cancel.
func TestWorkspaceConsumedKeyDisarmsCancel(t *testing.T) {
	m := openedChat(t)
	m.chat.client = &fakeTurnClient{}
	m.chat.submit("hi")

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	require.False(t, m.chat.turn.(*streamingTurn).cancelArmedAt.IsZero())
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})

	assert.True(t, m.chat.turn.(*streamingTurn).cancelArmedAt.IsZero())
}

// Reopening a session that holds a request shows its prompt, and the paused task once.
func TestWorkspaceRestoresThePendingRequest(t *testing.T) {
	m := openedChat(t)
	message := a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("approve?"))
	require.NoError(t, kagenta2a.AttachHITL(message, kagenta2a.ToolApprovalRequest{
		Type: kagenta2a.HITLTypeToolApprovalRequest, Tools: []kagenta2a.HITLTool{{ID: "approval-1", Name: "k8s_delete_pod"}},
	}))
	paused := &a2atype.Task{
		ID: "task-2", ContextID: "a",
		Status:  a2atype.TaskStatus{State: a2atype.TaskStateInputRequired, Message: message},
		History: []*a2atype.Message{a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("delete the pod"))},
	}
	done := historyTask("task-1", []*a2atype.Message{a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("hello"))})

	m.Update(sessionHistoryLoadedMsg{sessionID: "a", tasks: []*a2atype.Task{done, paused}, pending: paused})

	assert.Contains(t, m.chat.View(), "asking permission to run 1 tool")
	assert.Equal(t, 1, strings.Count(shownText(m.chat), "delete the pod"), "the paused task is shown once")
	assert.Less(t, strings.Index(shownText(m.chat), "hello"), strings.Index(shownText(m.chat), "delete the pod"))
	_, awaiting := m.chat.turn.(*awaitingTurn)
	assert.True(t, awaiting)
}

// A pending query that fails still shows the history it could read.
func TestWorkspaceHistoryWithAFailedPendingQuery(t *testing.T) {
	m := openedChat(t)
	done := historyTask("task-1", []*a2atype.Message{a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("hello"))})

	m.Update(sessionHistoryLoadedMsg{sessionID: "a", tasks: []*a2atype.Task{done}, err: errors.New("unavailable")})

	assert.Contains(t, shownText(m.chat), "hello")
	assert.Contains(t, m.status, "unavailable")
}

func TestWorkspaceRoutesDiscardResultsRegardlessOfFocus(t *testing.T) {
	m := openedChat(t)
	client := &fakeTurnClient{}
	m.chat.client = client
	m.chat.submit("hi")
	message := a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("waiting"))
	m.Update(streamMsg{gen: m.chat.streamGen, result: clia2a.StreamResult{Event: a2atype.NewStatusUpdateEvent(reqCtx(), a2atype.TaskStateInputRequired, message)}})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	_, cmd := m.Update(runes("y"))
	m.focus = panelSessions

	for _, msg := range runCmd(cmd) {
		m.Update(msg)
	}

	assert.Equal(t, []a2atype.TaskID{"task-1"}, client.cancels)
	assert.Contains(t, shownText(m.chat), "Discarded the request")
}

func TestWorkspaceRoutesPauseChecksRegardlessOfFocus(t *testing.T) {
	m := openedChat(t)
	client := &fakeTurnClient{getErr: errors.New("unavailable")}
	m.chat.client = client
	m.chat.submit("hi")
	m.Update(streamMsg{gen: m.chat.streamGen, result: clia2a.StreamResult{Event: a2atype.NewStatusUpdateEvent(reqCtx(), a2atype.TaskStateInputRequired, questionStatus(t, "ask-1", regionQuestion))}})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_, cmd := m.Update(streamMsg{gen: m.chat.streamGen, result: clia2a.StreamResult{Err: errors.New("connection reset")}})
	m.focus = panelSessions

	for _, msg := range runCmd(cmd) {
		m.Update(msg)
	}

	_, awaiting := m.chat.turn.(*awaitingTurn)
	assert.True(t, awaiting, "the request is shown again")
}

// An early send would race the restored request, so the workspace holds sends until history arrives.
func TestWorkspaceHoldsSendsUntilHistoryLoads(t *testing.T) {
	m := openedChat(t)
	client := &fakeTurnClient{}
	m.chat.client = client
	m.chat.input.SetValue("hello")
	require.True(t, m.chat.historyPending)

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Empty(t, client.sent)

	m.Update(sessionHistoryLoadedMsg{sessionID: "a", err: errors.New("unavailable")})
	assert.False(t, m.chat.historyPending, "a failed load still releases the composer")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Len(t, client.sent, 1)
}

// With no chat open the empty pane tells the user to pick another panel, so digits must work.
func TestWorkspaceDigitsSwitchPanelsWithoutAChat(t *testing.T) {
	m := testWorkspace(t, &fakeLister{pages: []*apiv1alpha1.ListSessionsResponse{page("")}})
	loaded(m)
	m.focus = panelChat
	require.Nil(t, m.chat)

	m.Update(runes("3"))

	assert.Equal(t, panelID(3), m.focus)
}
