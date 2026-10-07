package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
	clia2a "github.com/kagent-dev/kagent/go/core/cli/internal/a2a"
	sessionview "github.com/kagent-dev/kagent/go/core/cli/internal/tui/session"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/theme"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/transcript"
)

// SendMessageFn abstracts the A2A client's SendStreamingMessage method for easier testing.
type SendMessageFn func(ctx context.Context, req *a2atype.SendMessageRequest) <-chan clia2a.StreamResult

type streamDoneMsg struct{}

// chatMode says where keys go: the composer, or the transcript for folding entries.
type chatMode int

const (
	modeCompose chatMode = iota
	modeSelect
)

type chatModel struct {
	agentRef  string
	contextID string
	// The header is pinned above the viewport so it survives a long transcript.
	state      string
	lastActive time.Time
	verbose    bool

	vp    viewport.Model
	input textarea.Model

	// entries is the conversation. When textOpen, the last entry is the AgentText still being
	// assembled; appending anything else closes it.
	entries  []transcript.Entry
	textOpen bool
	// turnStart is where the current turn's entries begin. Call IDs repeat across turns, so
	// tool activity pairs and Visible filters only within a turn; earlier entries are sealed.
	turnStart int

	// folds is indexed by position in visibleEntries(). Appending leaves earlier positions alone;
	// positions shift only when Visible newly hides an earlier entry of the current turn.
	folds transcript.Folds
	mode  chatMode
	// selected is the visibleEntries() index under the cursor; meaningful only in modeSelect.
	selected int

	// projected is the assembler's last text projection, so cumulative chunks yield a delta not a duplicate.
	assembler *clia2a.Assembler
	projected string
	lastState a2atype.TaskState

	working    bool
	workStart  time.Time
	statusText string

	spin spinner.Model

	// ctx is the workspace's context, so cancelling the program cancels an in-flight stream.
	ctx       context.Context
	send      SendMessageFn
	streamCh  <-chan clia2a.StreamResult
	cancel    context.CancelFunc
	streaming bool
}

func newChatModel(ctx context.Context, agentRef string, contextID string, send SendMessageFn, verbose bool) *chatModel {
	input := textarea.New()
	input.Placeholder = "Type a message (Enter to send)"
	input.FocusedStyle.CursorLine = lipgloss.NewStyle()
	input.Prompt = "> "
	input.ShowLineNumbers = false
	input.SetHeight(1)
	input.Focus()

	vp := viewport.New(0, 0)
	vp.MouseWheelEnabled = true

	sp := spinner.New()
	sp.Spinner = spinner.Hamburger
	sp.Style = lipgloss.NewStyle().Foreground(theme.ColorPrimary)

	return &chatModel{
		ctx:       ctx,
		agentRef:  agentRef,
		contextID: contextID,
		verbose:   verbose,
		vp:        vp,
		input:     input,
		send:      send,
		spin:      sp,
	}
}

func (m *chatModel) Init() tea.Cmd {
	return m.spin.Tick
}

func (m *chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	var cmd tea.Cmd
	// The viewport's default keymap binds letters and space, which are composer text;
	// only page keys scroll it from the keyboard. Mouse and other messages pass through.
	if key, isKey := msg.(tea.KeyMsg); !isKey || key.Type == tea.KeyPgUp || key.Type == tea.KeyPgDown {
		m.vp, cmd = m.vp.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	switch msg := msg.(type) {
	case spinner.TickMsg:
		if m.working {
			var sCmd tea.Cmd
			m.spin, sCmd = m.spin.Update(msg)
			if sCmd != nil {
				cmds = append(cmds, sCmd)
			}
			return m, tea.Batch(cmds...)
		}
	case tickMsg:
		if m.working {
			m.updateStatus()
			return m, m.tick()
		}
		return m, nil
	case tea.WindowSizeMsg:
		// What View draws around the viewport: header and rule above; rule, status, and input below.
		const chromeHeight = 5
		vpHeight := max(msg.Height-chromeHeight, 5)

		oldWidth := m.vp.Width
		m.vp.Width = msg.Width
		m.vp.Height = vpHeight
		m.input.SetWidth(msg.Width)

		// Re-render content if width changed
		if oldWidth != msg.Width && msg.Width > 0 {
			m.render()
		}
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+o" {
			m.toggleAll()
			return m, nil
		}
		if m.mode == modeSelect {
			m.selectKey(msg)
			return m, nil
		}
		switch msg.String() {
		case "ctrl+g":
			m.enterSelect()
			return m, nil
		case "esc":
			// Never quits; the workspace owns ctrl+c.
			return m, nil
		case "enter":
			if m.streaming {
				return m, nil
			}
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			m.appendUser(text)
			m.input.Reset()
			return m, m.submit(text)
		}
	case clia2a.StreamResult:
		if msg.Err != nil {
			m.appendTransportError(msg.Err)
			m.endStream()
			return m, nil
		}
		m.appendEvent(msg.Event)
		return m, m.waitNext()
	case streamDoneMsg:
		m.endStream()
		return m, nil
	}

	m.input, cmd = m.input.Update(msg)
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m *chatModel) View() string {
	width := m.vp.Width
	if width <= 0 {
		width = 80 // default width if not yet sized
	}
	status := m.statusText
	if m.working {
		status = fmt.Sprintf("%s %s", m.spin.View(), status)
	}
	rule := theme.SeparatorStyle().Render(strings.Repeat("─", max(10, width)))
	if m.mode == modeSelect {
		status = theme.DimStyle().Render("select mode")
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		m.headerView(width),
		rule,
		m.vp.View(),
		rule,
		theme.StatusStyle().Render(status),
		m.input.View(),
	)
}

// setHeaderMeta adds lifecycle detail to the header; the caller pre-renders state to keep this type-free.
func (m *chatModel) setHeaderMeta(state string, lastActive time.Time) {
	m.state, m.lastActive = state, lastActive
}

// stop cancels an in-flight stream, so a replaced chat stops delivering.
func (m *chatModel) stop() {
	if m != nil && m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

// headerView pins who you are talking to; the ID is abbreviated so metadata survives a narrow pane.
func (m *chatModel) headerView(width int) string {
	parts := []string{
		theme.HeadingStyle().Render(m.agentRef),
		theme.DimStyle().Render(sessionview.ShortID(m.contextID)),
	}
	if m.state != "" {
		parts = append(parts, m.state)
	}
	if !m.lastActive.IsZero() {
		parts = append(parts, theme.DimStyle().Render("active "+sessionview.Since(m.lastActive, time.Now())+" ago"))
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(strings.Join(parts, theme.DimStyle().Render(" · ")))
}

func (m *chatModel) submit(text string) tea.Cmd {
	m.sealTurn()
	m.streaming = true
	m.assembler = &clia2a.Assembler{}
	m.projected = ""
	m.lastState = ""
	m.setWorkingTime(time.Time{})
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel

	msg := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart(text))
	msg.ContextID = m.contextID
	req := &a2atype.SendMessageRequest{Message: msg}

	m.streamCh = m.send(ctx, req)
	return tea.Batch(m.waitNext(), m.tick())
}

func (m *chatModel) waitNext() tea.Cmd {
	ch := m.streamCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		result, ok := <-ch
		if !ok {
			return streamDoneMsg{}
		}
		return result
	}
}

// endStream commits any in-flight agent text and clears the working state.
func (m *chatModel) endStream() {
	m.textOpen = false
	m.streaming = false
	m.working = false
	m.lastActive = time.Now()
	m.updateStatus()
}

// appendEvent reduces one stream event and renders what it changed.
func (m *chatModel) appendEvent(ev a2atype.Event) {
	if ev == nil {
		return
	}
	if m.assembler == nil {
		m.assembler = &clia2a.Assembler{}
	}
	if err := m.assembler.Apply(ev); err != nil {
		m.appendEntry(transcript.Banner{Kind: transcript.BannerError, Text: fmt.Sprintf("Protocol error: %v", err)})
		return
	}
	m.renderToolActivity(eventParts(ev))
	m.renderAssembledText()
	m.renderState()
}

// eventParts returns what one event carries, so tool activity shows as it happens.
func eventParts(ev a2atype.Event) a2atype.ContentParts {
	switch res := ev.(type) {
	case *a2atype.Message:
		return res.Parts
	case *a2atype.TaskStatusUpdateEvent:
		if res.Status.Message != nil {
			return res.Status.Message.Parts
		}
	case *a2atype.TaskArtifactUpdateEvent:
		if res.Artifact != nil {
			return res.Artifact.Parts
		}
	}
	return nil
}

// renderAssembledText appends newly assembled text; the cumulative projection grows the block in place.
func (m *chatModel) renderAssembledText() {
	text, err := assembledText(m.assembler.Result())
	if err != nil {
		m.appendTransportError(err)
		return
	}
	if text == m.projected {
		return
	}
	delta, extends := strings.CutPrefix(text, m.projected)
	m.projected = text
	if extends && m.textOpen {
		last := len(m.entries) - 1
		if open, ok := m.entries[last].(transcript.AgentText); ok {
			open.Text += delta
			m.entries[last] = open
			m.render()
			return
		}
	}
	if extends {
		text = delta
	}
	m.appendEntry(transcript.AgentText{Text: text})
	m.textOpen = true
}

// assembledText projects agent output; only artifacts carry it, status messages are control-plane.
func assembledText(result a2atype.SendMessageResult) (string, error) {
	switch result := result.(type) {
	case *a2atype.Message:
		return clia2a.PartsText(result.Parts)
	case *a2atype.Task:
		var groups []string
		for _, artifact := range result.Artifacts {
			if artifact == nil {
				continue
			}
			text, err := clia2a.PartsText(artifact.Parts)
			if err != nil {
				return "", err
			}
			if text != "" {
				groups = append(groups, text)
			}
		}
		return strings.Join(groups, "\n"), nil
	default:
		return "", nil
	}
}

// renderState banners a state change; completion needs none.
func (m *chatModel) renderState() {
	task, ok := m.assembler.Result().(*a2atype.Task)
	if !ok {
		return
	}
	state := task.Status.State
	if state == m.lastState {
		return
	}
	m.lastState = state

	switch state {
	// The gateway rejects a message carrying a TaskID, so a reply starts a new task rather than resuming.
	case a2atype.TaskStateInputRequired:
		m.appendEntry(transcript.Banner{Kind: transcript.BannerInfo, Text: "⏸ Input required. Resuming a paused task is not supported yet; a reply starts a new one."})
	case a2atype.TaskStateAuthRequired:
		m.appendEntry(transcript.Banner{Kind: transcript.BannerInfo, Text: "⏸ Authentication required. This task cannot continue here."})
	case a2atype.TaskStateFailed, a2atype.TaskStateRejected, a2atype.TaskStateCanceled:
		banner := fmt.Sprintf("✗ Task %s.", state)
		if task.Status.Message != nil {
			detail, err := clia2a.PartsText(task.Status.Message.Parts)
			if err != nil {
				m.appendTransportError(err)
			} else if strings.TrimSpace(detail) != "" {
				banner += " " + detail
			}
		}
		m.appendEntry(transcript.Banner{Kind: transcript.BannerError, Text: banner})
	}
	if state.Terminal() || state == a2atype.TaskStateInputRequired || state == a2atype.TaskStateAuthRequired {
		m.working = false
		m.updateStatus()
	} else if task.Status.Timestamp != nil {
		m.setWorkingTime(*task.Status.Timestamp)
	}
}

// appendHistoryTask replays a past task as it happened, tool activity included.
// ProjectTask already applies Visible within the task, so it arrives sealed.
func (m *chatModel) appendHistoryTask(task *a2atype.Task) {
	m.sealTurn()
	m.entries = append(m.entries, transcript.ProjectTask(task)...)
	m.turnStart = len(m.entries)
	m.render()
}

// sealTurn fixes the current turn's entries as shown and starts a new turn after them.
func (m *chatModel) sealTurn() {
	m.textOpen = false
	m.entries = append(m.entries[:m.turnStart], transcript.Visible(m.entries[m.turnStart:])...)
	m.turnStart = len(m.entries)
}

func (m *chatModel) appendUser(text string) {
	m.appendEntry(transcript.UserMessage{Text: text})
}

// appendTransportError reports a stream failure, distinct from a task the agent itself failed.
func (m *chatModel) appendTransportError(err error) {
	m.appendEntry(transcript.Banner{Kind: transcript.BannerError, Text: fmt.Sprintf("Connection error: %v", err)})
}

// renderToolActivity folds kagent tool data parts into the transcript; the reducer has no opinion about them.
func (m *chatModel) renderToolActivity(parts a2atype.ContentParts) {
	changed := false
	for _, part := range parts {
		if part == nil || part.Data() == nil {
			continue
		}
		if m.verbose {
			if metaJSON, err := json.Marshal(part.Metadata); err == nil {
				m.appendEntry(transcript.Banner{Kind: transcript.BannerInfo, Text: fmt.Sprintf("DEBUG: DataPart metadata: %s", metaJSON)})
			}
			if dataJSON, err := json.Marshal(part.Data()); err == nil {
				m.appendEntry(transcript.Banner{Kind: transcript.BannerInfo, Text: fmt.Sprintf("DEBUG: DataPart data: %s", dataJSON)})
			}
		}
		activity, ok := kagenta2a.ParseToolActivity(part)
		if !ok {
			continue
		}
		before := len(m.entries)
		// Clipped so an appended entry cannot alias the turn into the sealed entries' array.
		turn := transcript.ApplyToolActivity(slices.Clip(m.entries[m.turnStart:]), activity)
		m.entries = append(m.entries[:m.turnStart], turn...)
		if len(m.entries) > before {
			m.textOpen = false // a new entry follows the text being assembled
		}
		changed = true
	}
	if changed {
		m.render()
	}
}

// appendEntry adds an entry after everything shown, closing the text being assembled.
func (m *chatModel) appendEntry(entry transcript.Entry) {
	m.textOpen = false
	m.entries = append(m.entries, entry)
	m.render()
}

// visibleEntries is what the transcript shows: sealed entries as they are, the current turn filtered.
func (m *chatModel) visibleEntries() []transcript.Entry {
	return append(slices.Clip(m.entries[:m.turnStart]), transcript.Visible(m.entries[m.turnStart:])...)
}

// render redraws the viewport at its width; entries wrap themselves.
// In select mode the viewport follows the cursor instead of the newest entry.
func (m *chatModel) render() {
	visible := m.visibleEntries()
	m.selected = min(m.selected, max(len(visible)-1, 0))
	blocks := make([]string, 0, len(visible))
	// start and end are the first and last viewport line of the selected block.
	line, start, end := 0, 0, 0
	for i, entry := range visible {
		selected := m.mode == modeSelect && i == m.selected
		block := transcript.Render(entry, m.vp.Width, m.folds.Expanded(i), selected)
		blocks = append(blocks, block)
		if selected {
			start, end = line, line+lipgloss.Height(block)-1
		}
		line += lipgloss.Height(block) + 1 // the blank separator line
	}
	m.vp.SetContent(strings.Join(blocks, "\n\n"))
	if m.mode != modeSelect {
		m.vp.GotoBottom()
		return
	}
	switch {
	case start < m.vp.YOffset:
		m.vp.SetYOffset(start)
	case end >= m.vp.YOffset+m.vp.Height:
		// A block taller than the viewport shows its head.
		m.vp.SetYOffset(min(end-m.vp.Height+1, start))
	}
}

// toggleAll expands or collapses every entry's output.
func (m *chatModel) toggleAll() {
	m.folds.ToggleAll()
	m.render()
}

// enterSelect starts at the newest entry; with nothing to select the composer keeps the keys.
func (m *chatModel) enterSelect() {
	n := len(m.visibleEntries())
	if n == 0 {
		return
	}
	m.mode = modeSelect
	m.selected = n - 1
	m.input.Blur()
	m.render()
}

func (m *chatModel) leaveSelect() {
	m.mode = modeCompose
	m.input.Focus()
	m.render()
}

// selectKey moves over every visible entry (not only foldable ones, so the reader can
// read and later copy any block); toggling is meaningful for tool activity.
func (m *chatModel) selectKey(msg tea.KeyMsg) {
	last := len(m.visibleEntries()) - 1
	switch msg.String() {
	case "up", "k":
		m.selected = max(m.selected-1, 0)
	case "down", "j":
		m.selected = min(m.selected+1, last)
	case "g":
		m.selected = 0
	case "G":
		m.selected = last
	case "enter", " ", "space":
		m.folds.Toggle(m.selected)
	case "o":
		m.folds.ToggleAll()
	case "esc", "ctrl+g", "i":
		m.leaveSelect()
		return
	}
	m.render()
}

type tickMsg time.Time

func (m *chatModel) tick() tea.Cmd {
	if !m.working {
		return nil
	}
	return tea.Tick(1*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *chatModel) setWorkingTime(ts time.Time) {
	if !m.working {
		if !ts.IsZero() {
			m.workStart = ts
		} else {
			m.workStart = time.Now()
		}
	}
	m.working = true
	m.updateStatus()
}

func (m *chatModel) updateStatus() {
	if m.working {
		m.statusText = fmt.Sprintf("Working… %s", time.Since(m.workStart).Round(time.Second))
	} else {
		m.statusText = ""
	}
}
