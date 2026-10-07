package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kagent-dev/kagent/go/api/client"
	api "github.com/kagent-dev/kagent/go/api/httpapi"
	"github.com/kagent-dev/kagent/go/api/v1alpha2"
	"github.com/kagent-dev/kagent/go/core/cli/internal/config"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/dialogs"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/keys"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/theme"
	"github.com/kagent-dev/kagent/go/core/internal/utils"
	"github.com/kagent-dev/kagent/go/core/internal/version"
	a2aclient "trpc.group/trpc-go/trpc-a2a-go/client"
	"trpc.group/trpc-go/trpc-a2a-go/protocol"
)

// RunWorkspace launches a split-pane TUI: sessions (left), chat (center), details (toggleable right).
func RunWorkspace(cfg *config.Config, clientSet *client.ClientSet, verbose bool) error {
	out := newLockedOutput(os.Stdout)
	m := newWorkspaceModel(cfg, clientSet, verbose)
	m.clip = out
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithOutput(out))
	_, err := p.Run()
	return err
}

type focusArea int

const (
	focusSessions focusArea = iota
	focusChat
)

type loadSessionsMsg struct{ sessions []*api.Session }
type wsAgentsLoadedMsg struct {
	agents []api.AgentResponse
	err    error
}
type loadAgentMsg struct {
	agent api.AgentResponse
	err   error
}
type sessionSelectedMsg struct{ session *api.Session }
type sessionHistoryLoadedMsg struct {
	sessionID string
	items     []*protocol.Task
	err       error
}
type agentChosenMsg struct{ agent api.AgentResponse }
type createSessionMsg struct {
	session *api.Session
	err     error
}

type sessionListItem struct{ s *api.Session }

func (i sessionListItem) Title() string {
	if i.s.Name != nil {
		return *i.s.Name
	}
	return i.s.ID
}
func (i sessionListItem) Description() string { return i.s.ID }
func (i sessionListItem) FilterValue() string { return i.Title() }

type workspaceModel struct {
	cfg     *config.Config
	client  *client.ClientSet
	verbose bool

	width  int
	height int

	// panes
	sessions    list.Model
	chat        *chatModel
	details     strings.Builder
	showDetails bool

	// data
	agent    *api.AgentResponse
	agentRef string
	current  *api.Session
	agents   []api.AgentResponse

	// focus
	focus focusArea

	// create session
	naming       bool
	sessionInput textinput.Model

	// agent selection
	choosingAgent bool
	agentList     list.Model
	dlg           *dialogs.Manager

	// selectLayout hides the sidebar and details so the transcript can be drag-selected alone.
	selectLayout bool
	// clip receives copied text; nil means copying is unavailable.
	clip clipboardWriter

	// key map
	keys keys.KeyMap
	help help.Model
}

func newWorkspaceModel(cfg *config.Config, clientSet *client.ClientSet, verbose bool) *workspaceModel {
	sessionList := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	sessionList.Title = "Sessions"
	sessionList.SetShowStatusBar(false)
	sessionList.SetShowHelp(false)
	sessionList.SetFilteringEnabled(true)
	// q and esc would quit the whole TUI from the list; ctrl+c is the one quit key.
	sessionList.DisableQuitKeybindings()

	// Provide a sane default size before first WindowSizeMsg arrives
	sessionList.SetSize(30, 20)

	sessionTextInput := textinput.New()
	sessionTextInput.Placeholder = "New session name"
	sessionTextInput.Prompt = "> "

	agentList := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	agentList.Title = "Agents"
	agentList.SetShowStatusBar(false)
	agentList.SetShowHelp(false)
	agentList.SetFilteringEnabled(true)

	return &workspaceModel{
		cfg:          cfg,
		client:       clientSet,
		verbose:      verbose,
		sessions:     sessionList,
		focus:        focusChat,
		sessionInput: sessionTextInput,
		agentList:    agentList,
		keys:         keys.DefaultKeyMap(),
		dlg:          dialogs.NewManager(),
		help:         help.New(),
	}
}

func (m *workspaceModel) Init() tea.Cmd {
	return m.loadAgents()
}

func (m *workspaceModel) loadAgents() tea.Cmd {
	return func() tea.Msg {
		resp, err := m.client.Agent.ListAgents(context.Background())
		return wsAgentsLoadedMsg{agents: resp.Data, err: err}
	}
}

func (m *workspaceModel) loadSessions() tea.Cmd {
	return func() tea.Msg {
		resp, err := m.client.Session.ListSessions(context.Background())
		if err != nil {
			return loadSessionsMsg{sessions: nil}
		}
		// filter by agent
		filtered := make([]*api.Session, 0, len(resp.Data))
		for _, s := range resp.Data {
			if s.AgentID != nil && *s.AgentID == m.agent.ID {
				ss := s
				filtered = append(filtered, ss)
			}
		}
		return loadSessionsMsg{sessions: filtered}
	}
}

func (m *workspaceModel) createSession(name string) tea.Cmd {
	return func() tea.Msg {
		res, err := m.client.Session.CreateSession(context.Background(), &api.SessionRequest{
			Name:     new(name),
			AgentRef: new(m.agentRef),
		})
		if err != nil {
			return createSessionMsg{session: nil, err: err}
		}
		return createSessionMsg{session: res.Data, err: nil}
	}
}

func (m *workspaceModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Forward to dialog manager first; if a dialog is open, it captures input
	dcmd := m.dlg.Handle(msg)
	if m.dlg.HasDialogs() {
		// Only allow WindowSizeMsg to continue to adjust layout beneath
		if _, ok := msg.(tea.WindowSizeMsg); !ok {
			return m, dcmd
		}
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.Width = msg.Width
		if m.choosingAgent {
			m.agentList.SetSize(max(20, m.width/2), max(10, m.height/2))
		}
		if m.naming {
			m.sessionInput.Width = max(10, m.width/2-4)
		}
		return m, m.resize()
	case agentChosenMsg:
		a := msg.agent
		m.agent = &a
		m.agentRef = utils.ConvertToKubernetesIdentifier(a.ID)
		// Clear current session and chat when switching agents
		m.current = nil
		m.chat = nil
		m.focus = focusSessions
		m.renderDetails()
		return m, m.loadSessions()
	case wsAgentsLoadedMsg:
		if msg.err != nil {
			fmt.Fprintf(&m.details, "Error: failed to load agents: %v", msg.err)
			return m, nil
		}
		// Sort and store agents for later; do not auto-open chooser or auto-select.
		slices.SortStableFunc(msg.agents, func(a, b api.AgentResponse) int {
			return strings.Compare(
				utils.ResourceRefString(a.Agent.Metadata.Namespace, a.Agent.Metadata.Name),
				utils.ResourceRefString(b.Agent.Metadata.Namespace, b.Agent.Metadata.Name),
			)
		})
		m.agents = msg.agents
		// Keep welcome screen visible until user presses Ctrl+A
		return m, nil
	case loadAgentMsg:
		if msg.err != nil {
			fmt.Fprintf(&m.details, "Error: %v", msg.err)
			return m, nil
		}
		a := msg.agent
		m.agent = &a
		m.agentRef = utils.ConvertToKubernetesIdentifier(a.ID)
		// Clear current session and chat when switching agents
		m.current = nil
		m.chat = nil
		m.focus = focusSessions
		m.renderDetails()
		return m, m.loadSessions()
	case loadSessionsMsg:
		// Sort sessions by UpdatedAt then CreatedAt (newest first)
		sortSessions(msg.sessions)
		items := make([]list.Item, 0, len(msg.sessions))
		for _, s := range msg.sessions {
			items = append(items, sessionListItem{s: s})
		}
		m.sessions.SetItems(items)
		// If sessions list hasn't been sized yet, set a default for visibility
		if m.sessions.Width() == 0 || m.sessions.Height() == 0 {
			w := m.width
			h := m.height
			if w == 0 {
				w = 80
			}
			if h == 0 {
				h = 24
			}
			m.sessions.SetSize(w, h)
		}
		if len(msg.sessions) > 0 {
			m.sessions.Select(0)
			return m, func() tea.Msg { return sessionSelectedMsg{session: msg.sessions[0]} }
		}
		// No sessions: open naming dialog immediately
		m.naming = true
		m.sessionInput.SetValue("")
		m.sessionInput.Focus()
		return m, nil
	case sessionSelectedMsg:
		// Clear chat, size chat, and show input
		m.current = msg.session
		m.focus = focusChat
		if m.chat != nil {
			m.chat.SetInputVisible(true)
		}
		return m, m.startChat(true)
	case sessionHistoryLoadedMsg:
		if m.chat != nil && m.current != nil && msg.sessionID == m.current.ID {
			m.chat.loadHistory(msg.items, msg.err)
		}
		return m, nil
	case createSessionMsg:
		m.naming = false
		if msg.err != nil {
			m.details.WriteString("\nFailed to create session\n")
			return m, nil
		}
		// prepend new session and select
		items := append([]list.Item{sessionListItem{s: msg.session}}, m.sessions.Items()...)
		m.sessions.SetItems(items)
		m.sessions.Select(0)
		m.current = msg.session
		// Start fresh chat for new session without loading any history
		return m, m.startChat(false)
	case tea.KeyMsg:
		s := msg.String()
		// Global keys
		if key.Matches(msg, m.keys.Quit) {
			return m, tea.Quit
		}
		if key.Matches(msg, m.keys.Sessions) {
			if m.agent == nil {
				return m, nil
			}
			m.naming = true
			m.sessionInput.SetValue("")
			m.sessionInput.Focus()
			return m, nil
		}
		if key.Matches(msg, m.keys.Agents) {
			if len(m.agents) == 0 {
				return m, m.loadAgents()
			}
			items := make([]*dialogs.AgentItem, 0, len(m.agents))
			for _, a := range m.agents {
				items = append(items, &dialogs.AgentItem{AgentResponse: a})
			}
			open := dialogs.OpenMsg{Model: dialogs.NewAgentChooser(items, func(it list.Item) tea.Cmd {
				return func() tea.Msg {
					if wi, ok := it.(*dialogs.AgentItem); ok {
						return agentChosenMsg{agent: wi.AgentResponse}
					}
					return nil
				}
			})}
			return m, func() tea.Msg { return open }
		}
		// Dialog-level esc handling
		if m.naming && s == "esc" {
			m.naming = false
			return m, nil
		}
		if m.choosingAgent && s == "esc" {
			m.choosingAgent = false
			return m, nil
		}
		switch s {
		case "tab":
			if m.naming || m.selectLayout || m.agent == nil {
				return m, nil
			}
			if m.focus == focusChat {
				m.focus = focusSessions
			} else {
				m.focus = focusChat
			}
			if m.chat != nil {
				m.chat.SetFocused(m.focus == focusChat)
			}
			return m, nil
		case "ctrl+d":
			m.showDetails = !m.showDetails
			return m, m.resize()
		case "ctrl+l":
			if m.naming || m.chat == nil {
				return m, nil
			}
			m.selectLayout = !m.selectLayout
			if m.selectLayout {
				m.focus = focusChat
				m.chat.SetFocused(true)
			}
			return m, m.resize()
		case "ctrl+o":
			// Folding is global: it applies to the transcript whichever pane has focus.
			if m.chat != nil {
				m.chat.toggleAll()
			}
			return m, nil
		case "enter":
			if m.naming {
				name := strings.TrimSpace(m.sessionInput.Value())
				if name != "" {
					return m, m.createSession(name)
				}
				m.naming = false
				return m, nil
			}
		}
		// A key goes to one place: the naming modal, the session list, or the chat.
		return m, m.routeKey(msg)
	}

	var cmds []tea.Cmd
	if m.choosingAgent {
		var cmd tea.Cmd
		m.agentList, cmd = m.agentList.Update(msg)
		if km, ok := msg.(tea.KeyMsg); ok && km.String() == "enter" {
			if it, ok := m.agentList.SelectedItem().(*dialogs.AgentItem); ok {
				m.choosingAgent = false
				a := it.AgentResponse
				m.agent = &a
				m.agentRef = utils.ConvertToKubernetesIdentifier(a.ID)
				// Clear current session and chat when switching agents
				m.current = nil
				m.chat = nil
				m.focus = focusSessions
				m.renderDetails()
				return m, m.loadSessions()
			}
		}
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	}
	// Other messages (stream events, ticks, cursor blinks) reach every pane.
	if m.focus == focusSessions {
		var cmd tea.Cmd
		m.sessions, cmd = m.sessions.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if m.chat != nil {
		_, cmd := m.chat.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if m.naming {
		var cmd tea.Cmd
		m.sessionInput, cmd = m.sessionInput.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Batch(cmds...)
}

// routeKey sends a key to the one pane that owns it.
func (m *workspaceModel) routeKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case m.naming:
		var cmd tea.Cmd
		m.sessionInput, cmd = m.sessionInput.Update(msg)
		return cmd
	case m.focus == focusSessions:
		if msg.String() == "enter" && m.sessions.FilterState() != list.Filtering {
			if it, ok := m.sessions.SelectedItem().(sessionListItem); ok {
				return func() tea.Msg { return sessionSelectedMsg{session: it.s} }
			}
		}
		var cmd tea.Cmd
		m.sessions, cmd = m.sessions.Update(msg)
		return cmd
	case m.chat != nil:
		if msg.String() == "ctrl+c" {
			return tea.Quit // the chat's own quit would leave the workspace's state behind
		}
		_, cmd := m.chat.Update(msg)
		return cmd
	}
	return nil
}

func (m *workspaceModel) resize() tea.Cmd {
	if m.width == 0 || m.height == 0 {
		return nil
	}
	// Compute available height excluding header and footer, to avoid clipping
	headerLines := lineCount(renderTitle(m.width))
	helpView := m.help.View(m.keys)
	footerLines := lineCount(helpView)
	availableHeight := max(m.height-headerLines-footerLines, 1)
	sidebarWidth, detailsWidth := m.paneWidths()
	centerWidth := max(m.width-sidebarWidth-detailsWidth, 20)

	m.sessions.SetSize(30, availableHeight)
	if m.chat != nil {
		// send adjusted size to chat
		_, cmd := m.chat.Update(tea.WindowSizeMsg{Width: centerWidth, Height: availableHeight})
		return cmd
	}
	return nil
}

func (m *workspaceModel) startChat(loadHistory bool) tea.Cmd {
	if m.agent == nil || m.current == nil {
		return nil
	}
	a2aPath := "api/a2a"
	if m.agent != nil && m.agent.WorkloadMode == v1alpha2.WorkloadModeSandbox {
		a2aPath = "api/a2a-sandboxes"
	}
	a2aURL := fmt.Sprintf("%s/%s/%s", m.cfg.KAgentURL, a2aPath, m.agentRef)
	client, err := a2aclient.NewA2AClient(a2aURL,
		a2aclient.WithTimeout(m.cfg.Timeout),
	)
	if err != nil {
		m.details.WriteString("\nA2A error\n")
		return nil
	}
	sendFn := func(ctx context.Context, params protocol.SendMessageParams) (<-chan protocol.StreamingMessageEvent, error) {
		return client.StreamMessage(ctx, params)
	}
	// Reset chat for new session; a stream of the previous one stops delivering.
	m.chat.stopStream()
	m.chat = newChatModel(m.agentRef, m.current.ID, sendFn, m.verbose)
	m.chat.clip = m.clip
	m.chat.historyPending = loadHistory
	m.chat.SetFocused(m.focus == focusChat)
	// Set header and clear transcript
	title := theme.HeadingStyle().Render(fmt.Sprintf("Chat with %s (session %s)", m.agentRef, m.current.ID))
	m.chat.ResetTranscript(title)
	// Ensure chat viewport is sized immediately and optionally fetch history
	if loadHistory {
		return tea.Batch(m.resize(), m.fetchSessionHistoryCmd(m.current.ID))
	}
	return m.resize()
}

func (m *workspaceModel) fetchSessionHistoryCmd(sessionID string) tea.Cmd {
	return func() tea.Msg {
		tasksURL := fmt.Sprintf("%s/api/sessions/%s/tasks?user_id=%s", m.cfg.KAgentURL, sessionID, "admin@kagent.dev")
		resp, err := http.Get(tasksURL) //nolint:gosec
		if err != nil {
			return sessionHistoryLoadedMsg{sessionID: sessionID, err: err}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return sessionHistoryLoadedMsg{sessionID: sessionID, err: fmt.Errorf("listing tasks: %s", resp.Status)}
		}
		var payload struct {
			Data []*protocol.Task `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			return sessionHistoryLoadedMsg{sessionID: sessionID, err: err}
		}
		return sessionHistoryLoadedMsg{sessionID: sessionID, items: payload.Data}
	}
}

// renderDetails creates the contents for the right pane
func (m *workspaceModel) renderDetails() {
	if m.agent == nil {
		return
	}
	m.details.Reset()
	fmt.Fprintf(&m.details, "Agent: %s\n", utils.ConvertToKubernetesIdentifier(m.agent.ID))
	if m.agent.Agent.Spec.Description != "" {
		fmt.Fprintf(&m.details, "\n%s\n", m.agent.Agent.Spec.Description)
	}
	// Tools information (if declarative tools are present)
	if m.agent.Agent != nil && m.agent.Agent.Spec.Declarative != nil && len(m.agent.Agent.Spec.Declarative.Tools) > 0 {
		fmt.Fprintf(&m.details, "\nTools:\n")
		for _, t := range m.agent.Agent.Spec.Declarative.Tools {
			switch t.Type {
			case v1alpha2.ToolProviderType_McpServer:
				name := ""
				if t.McpServer != nil {
					name = t.McpServer.Name
				}
				fmt.Fprintf(&m.details, "- MCP Server: %s", name)
				if t.McpServer != nil && len(t.McpServer.ToolNames) > 0 {
					fmt.Fprintf(&m.details, " (tools: %s)", strings.Join(t.McpServer.ToolNames, ", "))
				}
				fmt.Fprint(&m.details, "\n")
			case v1alpha2.ToolProviderType_Agent:
				name := ""
				if t.Agent != nil {
					name = t.Agent.Name
				}
				fmt.Fprintf(&m.details, "- Agent tool: %s\n", name)
			default:
				fmt.Fprintf(&m.details, "- Tool: (unknown type)\n")
			}
		}
	}
}

func (m *workspaceModel) View() string {
	// layout: left sessions, center chat, right details (optional)
	sidebarWidth, detailsWidth := m.paneWidths()

	// If any dialog is active, render only the dialog overlay (full screen)
	if v, ok := m.dlg.ViewOverlay(); ok {
		return v
	}

	// Left
	var left string
	if m.agent == nil || sidebarWidth == 0 {
		// No agent select, no sessions sidebar
		left = ""
	} else {
		if len(m.sessions.Items()) > 0 {
			// We only render sessions sidebar if we have an agent selected AND have sessions
			m.styleSessions(m.focus == focusSessions)
			left = lipgloss.NewStyle().Width(sidebarWidth).BorderForeground(theme.ColorBorder).Render(m.sessions.View())
		}
	}

	// Center
	// Use full width if left sidebar isn't rendered
	hasLeft := left != ""
	centerWidth := m.width - detailsWidth
	if hasLeft {
		centerWidth -= sidebarWidth
	}
	centerStyled := lipgloss.NewStyle().Width(centerWidth).Render(func() string {
		if m.agent == nil {
			// Start page: show instructions to select an agent
			boxWidth := min(centerWidth-4, 72)
			if boxWidth < 40 {
				boxWidth = max(20, centerWidth-4)
			}
			box := lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(theme.ColorBorder).
				Padding(1, 1).
				MaxWidth(boxWidth).Render(
				lipgloss.JoinVertical(lipgloss.Center,
					lipgloss.NewStyle().Bold(true).Render("Welcome to kagent!"),
					"",
					lipgloss.NewStyle().Foreground(theme.ColorMuted).Render("Use CTRL+A to select an agent and get started."),
					"",
					lipgloss.NewStyle().Foreground(theme.ColorPrimary).Render(`
    ████████████▄
  ████████████████▄
███               █▄
███    ██   ██    ██
▀██               ██
  ▀███████████████`),
					"",
					"",
					lipgloss.NewStyle().Foreground(theme.ColorMuted).Render("Website: https://kagent.dev"),
					lipgloss.NewStyle().Foreground(theme.ColorMuted).Render("Discord: http://bit.ly/kagentdiscord"),
				),
			)
			return lipgloss.Place(centerWidth, m.height-6, lipgloss.Center, lipgloss.Center, box)
		}
		if m.chat != nil {
			return m.chat.View()
		}
		if m.agent != nil && len(m.sessions.Items()) == 0 {
			// Agent selected but no sessions yet
			boxWidth := min(centerWidth-4, 72)
			if boxWidth < 40 {
				boxWidth = max(20, centerWidth-4)
			}
			box := lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(theme.ColorBorder).
				Padding(1, 2).
				MaxWidth(boxWidth).
				Render(
					lipgloss.JoinVertical(lipgloss.Left,
						lipgloss.NewStyle().Bold(true).Render("No sessions yet"),
						"",
						"Press Ctrl+N to create a new session.",
					),
				)
			return lipgloss.Place(centerWidth, max(10, m.height-6), lipgloss.Center, lipgloss.Center, box)
		}
		return lipgloss.NewStyle().Foreground(theme.ColorMuted).Render("Select a session to start chatting…")
	}())

	// Right (agent details)
	right := ""
	if m.agent != nil && detailsWidth > 0 {
		right = lipgloss.NewStyle().
			Width(detailsWidth).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(theme.ColorBorder).
			Padding(0, 1).Render(m.details.String())
	}

	// Compose rows with header and footer
	rowParts := []string{centerStyled}
	if left != "" {
		rowParts = append([]string{left, centerStyled}, rowParts[len(rowParts):]...)
	}
	if right != "" {
		rowParts = append(rowParts, right)
	}
	mainRow := lipgloss.JoinHorizontal(lipgloss.Top, rowParts...)
	logo := lipgloss.NewStyle().Bold(true).Foreground(theme.ColorPrimary).Render(renderTitle(m.width))

	// Display help at the bottom
	helpView := m.help.View(m.keys)
	footer := lipgloss.NewStyle().Foreground(theme.ColorMuted).Render(helpView)

	// Force main area height so footer stays pinned at bottom
	headerLines := lineCount(logo)
	footerLines := lineCount(footer)
	available := max(m.height-headerLines-footerLines, 1)
	mainRow = lipgloss.NewStyle().Height(available).Render(mainRow)
	content := lipgloss.JoinVertical(lipgloss.Left, logo, mainRow, footer)

	if m.choosingAgent {
		// Dialog manager now owns agent chooser view; render only dialog overlay
		if v, ok := m.dlg.ViewOverlay(); ok {
			return v
		}
	}
	if m.naming {
		w := m.width
		h := m.height
		if w == 0 {
			w = 80
		}
		if h == 0 {
			h = 24
		}
		modalWidth := min(max(w/2, 40), w-6)
		// ensure input fits the modal
		m.sessionInput.Width = max(10, modalWidth-4)
		modal := lipgloss.NewStyle().Width(modalWidth).Border(lipgloss.RoundedBorder()).BorderForeground(theme.ColorBorder).Padding(1, 2).Render(
			lipgloss.JoinVertical(lipgloss.Left,
				lipgloss.NewStyle().Bold(true).Render("New Session"),
				"",
				m.sessionInput.View(),
			),
		)
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, modal)
	}
	return content
}

// styleSessions highlights the session list's title while it has focus and dims its
// selection while it does not, so tab's effect is visible.
func (m *workspaceModel) styleSessions(focused bool) {
	title := list.DefaultStyles().Title
	delegate := list.NewDefaultDelegate()
	if focused {
		title = title.Background(theme.ColorPrimary).Foreground(lipgloss.Color("#FFFFFF"))
	} else {
		title = title.UnsetBackground().Foreground(theme.ColorMuted)
		delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.Foreground(theme.ColorMuted).BorderForeground(theme.ColorMuted)
		delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.Foreground(theme.ColorMuted).BorderForeground(theme.ColorMuted)
	}
	m.sessions.Styles.Title = title
	m.sessions.SetDelegate(delegate)
}

// paneWidths are the sidebar's and details' widths; the select layout hides both.
func (m *workspaceModel) paneWidths() (sidebar, details int) {
	if m.selectLayout {
		return 0, 0
	}
	if m.showDetails {
		return 30, 32
	}
	return 30, 0
}

// renderTitle returns a styled block-art banner for the header.
func renderTitle(width int) string {
	_ = width
	ver := version.GetVersion()
	title := fmt.Sprintf("kagent  %s", lipgloss.NewStyle().Foreground(theme.ColorMuted).Render(ver))
	return title
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := 1
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			n++
		}
	}
	return n
}

// sortSessions sorts sessions by UpdatedAt then CreatedAt descending.
func sortSessions(sessions []*api.Session) {
	slices.SortStableFunc(sessions, func(i, j *api.Session) int {
		if i.UpdatedAt.After(j.UpdatedAt) {
			return 1
		}
		if j.UpdatedAt.After(i.UpdatedAt) {
			return -1
		}
		if i.CreatedAt.After(j.CreatedAt) {
			return 1
		}
		if j.CreatedAt.After(i.CreatedAt) {
			return -1
		}
		return 0
	})
}
