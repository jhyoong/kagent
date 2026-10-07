package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/hitl"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/theme"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/transcript"
	"trpc.group/trpc-go/trpc-a2a-go/protocol"
)

// SendMessageFn abstracts the A2A client's StreamMessage method for easier testing.
type SendMessageFn func(ctx context.Context, params protocol.SendMessageParams) (<-chan protocol.StreamingMessageEvent, error)

// RunChat starts the TUI chat, blocking until the user exits.
func RunChat(agentRef string, sessionID string, sendFn SendMessageFn, verbose bool) error {
	out := newLockedOutput(os.Stdout)
	model := newChatModel(agentRef, sessionID, sendFn, verbose)
	model.clip = out
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithOutput(out))
	_, err := p.Run()
	return err
}

// chatMode says where keys go: the composer, or the transcript for folding and copying entries.
type chatMode int

const (
	modeCompose chatMode = iota
	modeSelect
)

type chatModel struct {
	agentRef  string
	sessionID string
	verbose   bool

	vp    viewport.Model
	input textarea.Model
	// height is the whole chat's; the viewport gets what the status line and bottom block leave.
	height int

	title string
	log   *transcript.Log
	folds transcript.Folds
	mode  chatMode
	// selected is the entry under the cursor; meaningful only in modeSelect.
	selected int

	working    bool
	workStart  time.Time
	statusText string
	// note is a one-off status such as "copied 12 bytes"; the next key clears it.
	note string

	// clip receives copied text; nil means copying is unavailable.
	clip clipboardWriter

	spin spinner.Model

	send SendMessageFn
	// turn is never nil: idleTurn, *streamingTurn or *awaitingTurn.
	turn turnState
	// streamGen numbers streams, so a message from an abandoned stream is recognised as stale.
	streamGen uint64
	// historyPending is true from opening a session until its history, and any
	// request it holds, has arrived; a send before then could race the restored request.
	historyPending bool

	showInput bool
	// focused marks the pane the workspace sends keys to.
	focused bool
	// scrolledBack is true while the reader has paged up from the newest output;
	// new output then leaves the view where it is.
	scrolledBack bool
}

func newChatModel(agentRef string, sessionID string, send SendMessageFn, verbose bool) *chatModel {
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

	m := &chatModel{
		agentRef:  agentRef,
		sessionID: sessionID,
		verbose:   verbose,
		vp:        vp,
		input:     input,
		send:      send,
		title:     fmt.Sprintf("Chat with %s (session %s)", agentRef, sessionID),
		log:       transcript.NewLog(),
		turn:      idleTurn{},
		spin:      sp,
		showInput: true,
	}
	m.render()
	return m
}

func (m *chatModel) Init() tea.Cmd {
	return m.spin.Tick
}

func (m *chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	m.layout() // the bottom block may have changed height
	return m, cmd
}

func (m *chatModel) update(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	var cmd tea.Cmd
	// The viewport's default keymap binds letters and space, which are composer text;
	// only page keys scroll it from the keyboard. Other messages pass through.
	if key, isKey := msg.(tea.KeyMsg); !isKey || key.Type == tea.KeyPgUp || key.Type == tea.KeyPgDown {
		m.vp, cmd = m.vp.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		if isKey {
			m.scrolledBack = !m.vp.AtBottom()
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
		}
		return tea.Batch(cmds...)
	case tickMsg:
		if m.working {
			m.updateStatus()
			return m.tick()
		}
		return nil
	case tea.WindowSizeMsg:
		oldWidth := m.vp.Width
		m.vp.Width = msg.Width
		m.height = msg.Height
		m.input.SetWidth(msg.Width)
		if oldWidth != msg.Width && msg.Width > 0 {
			m.render()
		}
		return nil
	case tea.KeyMsg:
		return m.key(msg)
	case streamMsg:
		if m.isCurrentStream(msg.gen) {
			m.applyEvent(msg.event)
			return m.waitNext()
		}
		return nil
	case streamDoneMsg:
		if m.isCurrentStream(msg.gen) {
			m.endStream()
		}
		return nil
	case stopDisarmMsg:
		if turn, ok := m.turn.(*streamingTurn); ok && turn.stopArmedAt.Equal(msg.armedAt) {
			turn.stopArmedAt = time.Time{}
		}
		return nil
	case exportDoneMsg:
		m.applyExportDone(msg)
		return nil
	}
	return tea.Batch(cmds...)
}

// key routes one key: global chat keys first, then select mode, a paused turn's
// prompt, or the composer.
func (m *chatModel) key(msg tea.KeyMsg) tea.Cmd {
	if msg.Type != tea.KeyEsc {
		m.disarmStop()
	}
	m.note = ""
	switch msg.String() {
	case "ctrl+c":
		m.stopStream()
		return tea.Quit
	case "ctrl+o":
		m.toggleAll()
		return nil
	}
	if m.mode == modeSelect {
		return m.selectKey(msg)
	}
	switch msg.String() {
	case "ctrl+g":
		m.enterSelect()
		return nil
	case "ctrl+y":
		// Copying the last answer is harmless while a prompt replaces the composer, so it is not gated.
		m.copyLastAnswer()
		return nil
	}
	if turn, ok := m.turn.(*awaitingTurn); ok {
		return m.awaitKey(turn, msg)
	}
	switch msg.String() {
	case "esc":
		// Never quits. Twice in a row stops listening to the running turn.
		return m.pressEsc()
	case "enter":
		if !m.showInput {
			return nil
		}
		if m.isStreaming() {
			return nil
		}
		if m.historyPending {
			m.note = "loading history…"
			return nil
		}
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return nil
		}
		m.input.Reset()
		return m.submit(text)
	}
	if msg.Type == tea.KeyRunes {
		// Runes typed in a burst arrive as one message; a chunk such as "up" would
		// otherwise match the textarea's arrow-key bindings and be dropped.
		m.input.InsertString(string(msg.Runes))
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
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
	if turn, ok := m.turn.(*streamingTurn); ok && !turn.stopArmedAt.IsZero() {
		status += "  press esc again to stop listening"
	}
	if m.scrolledBack {
		status = strings.TrimSpace(status + "  scrolled back · pgdn to follow")
	}
	if m.mode == modeSelect {
		status = "select mode · ↑↓ move · enter toggle · o all · y copy · Y copy all · e export · esc back"
	}
	if m.note != "" {
		status = m.note
	}
	rule := theme.SeparatorStyle().Render(strings.Repeat("─", max(10, width)))
	if m.focused {
		rule = theme.FocusStyle().Render(strings.Repeat("━", max(10, width)))
	}
	parts := []string{
		m.vp.View(),
		rule,
		theme.StatusStyle().Render(ansi.Truncate(status, width, "…")),
	}
	if bottom := m.bottomView(width); bottom != "" {
		parts = append(parts, bottom)
	}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// bottomView is the composer, or the prompt of a paused turn with its hint line.
func (m *chatModel) bottomView(width int) string {
	turn, ok := m.turn.(*awaitingTurn)
	if !ok {
		if !m.showInput {
			return ""
		}
		return m.input.View()
	}
	hints := turn.prompt.hints()
	if turn.discard == discardConfirming {
		hints = "Reject this request? The agent continues without it.  y reject · n keep"
	}
	return turn.prompt.view(width) + "\n" + wrapLines([]string{theme.DimStyle().Render(hints)}, width)
}

// layout gives the viewport the height the status line and bottom block leave,
// measured because a prompt is taller than the composer and changes as it is used.
func (m *chatModel) layout() {
	if m.height <= 0 {
		return
	}
	// The rule and status line sit between the transcript and the bottom block.
	chrome := 2
	if bottom := m.bottomView(m.vp.Width); bottom != "" {
		chrome += lipgloss.Height(bottom)
	}
	if height := max(m.height-chrome, 5); height != m.vp.Height {
		m.vp.Height = height
		m.render()
	}
}

func (m *chatModel) submit(text string) tea.Cmd {
	m.log.AddUserMessage(text)
	m.scrolledBack = false
	m.render()
	sessionID := m.sessionID
	message := protocol.NewMessageWithContext(protocol.MessageRoleUser, []protocol.Part{protocol.NewTextPart(text)}, nil, &sessionID)
	return m.startStream(message, nil)
}

// startStream sends message in this chat's session and makes it the running turn.
// resumes is the request message answers; if the send fails it is shown again.
func (m *chatModel) startStream(message protocol.Message, resumes *hitl.Pending) tea.Cmd {
	m.stopStream()
	ctx, stop := context.WithCancel(context.Background())
	ch, err := m.send(ctx, protocol.SendMessageParams{Message: message})
	if err != nil {
		stop()
		m.appendEntry(transcript.Banner{Kind: transcript.BannerError, Text: fmt.Sprintf("Error: %v", err)})
		if resumes != nil {
			m.await(*resumes)
		}
		return nil
	}
	m.streamGen++
	m.turn = &streamingTurn{gen: m.streamGen, ch: ch, stop: stop}
	m.setWorkingTime(time.Time{})
	return tea.Batch(m.waitNext(), m.tick())
}

func (m *chatModel) waitNext() tea.Cmd {
	turn, ok := m.turn.(*streamingTurn)
	if !ok {
		return nil
	}
	ch, gen := turn.ch, turn.gen
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return streamDoneMsg{gen: gen}
		}
		return streamMsg{gen: gen, event: ev}
	}
}

// stopStream drops the running stream's connection, if any, and leaves the turn idle.
// The task itself runs on: the runtimes do not all support cancelling it.
func (m *chatModel) stopStream() {
	if m == nil {
		return
	}
	if turn, ok := m.turn.(*streamingTurn); ok && turn.stop != nil {
		turn.stop()
	}
	m.turn = idleTurn{}
}

// endStream closes the finished stream; a task that paused shows its request.
func (m *chatModel) endStream() {
	turn, ok := m.turn.(*streamingTurn)
	m.stopStream()
	m.working = false
	m.updateStatus()
	if ok && turn.paused != nil {
		m.await(*turn.paused)
	}
}

func (m *chatModel) isStreaming() bool {
	_, ok := m.turn.(*streamingTurn)
	return ok
}

// isCurrentStream reports whether a stream message tagged gen belongs to the running turn.
func (m *chatModel) isCurrentStream(gen uint64) bool {
	turn, ok := m.turn.(*streamingTurn)
	return ok && turn.gen == gen
}

// applyEvent adds what one stream event shows. Agent prose and tool activity
// arrive in working status messages; the final artifact repeats the last of
// them; an input-required status holds the request the task paused on.
func (m *chatModel) applyEvent(ev protocol.StreamingMessageEvent) {
	turn, ok := m.turn.(*streamingTurn)
	if !ok {
		return
	}
	if m.verbose {
		if raw, err := json.Marshal(ev.Result); err == nil {
			m.log.Append(transcript.Banner{Kind: transcript.BannerInfo, Text: "DEBUG: " + string(raw)})
		}
	}
	switch res := ev.Result.(type) {
	case *protocol.TaskStatusUpdateEvent:
		m.applyStatus(turn, res.TaskID, res.Status, res.Final, res.Metadata)
	case *protocol.TaskArtifactUpdateEvent:
		if res.LastChunk != nil && *res.LastChunk {
			m.log.ApplyArtifact(res.Artifact.Parts)
		}
	case *protocol.Message:
		m.log.ApplyMessage(*res, false)
	case *protocol.Task:
		for _, artifact := range res.Artifacts {
			m.log.ApplyArtifact(artifact.Parts)
		}
		m.applyStatus(turn, res.ID, res.Status, true, res.Metadata)
	}
	m.render()
}

func (m *chatModel) applyStatus(turn *streamingTurn, taskID string, status protocol.TaskStatus, final bool, metadata map[string]any) {
	message := status.Message
	switch status.State {
	case protocol.TaskStateInputRequired:
		if message != nil {
			if request, ok := hitl.ReadRequest(message.Parts); ok {
				turn.paused = &hitl.Pending{TaskID: taskID, Request: request}
			}
		}
		m.working = false
		m.updateStatus()
		return
	case protocol.TaskStateFailed, protocol.TaskStateRejected, protocol.TaskStateCanceled:
		banner := fmt.Sprintf("✗ Task %s.", status.State)
		if message != nil {
			if detail := strings.TrimSpace(transcript.TextOf(message.Parts)); detail != "" {
				banner += " " + detail
			}
		}
		m.log.Append(transcript.Banner{Kind: transcript.BannerError, Text: banner})
		return
	case protocol.TaskStateAuthRequired:
		m.log.Append(transcript.Banner{Kind: transcript.BannerInfo, Text: "⏸ Authentication required. This task cannot continue here."})
		return
	case protocol.TaskStateWorking, protocol.TaskStateSubmitted:
		if ts, err := time.Parse(time.RFC3339Nano, status.Timestamp); err == nil {
			m.setWorkingTime(ts)
		}
	}
	if message == nil {
		return
	}
	if final {
		// A final message repeats what the turn has shown, like the final artifact.
		m.log.ApplyArtifact(message.Parts)
		return
	}
	if message.Role == protocol.MessageRoleAgent {
		m.log.ApplyMessage(*message, transcript.IsPartial(message.Metadata) || transcript.IsPartial(metadata))
	}
}

// pressEsc arms on the first esc of a running turn and stops listening on the second.
func (m *chatModel) pressEsc() tea.Cmd {
	turn, ok := m.turn.(*streamingTurn)
	if !ok {
		return nil
	}
	if !turn.stopArmedAt.IsZero() && time.Since(turn.stopArmedAt) <= stopArmWindow {
		m.stopStream()
		m.working = false
		m.updateStatus()
		m.appendEntry(transcript.Banner{Kind: transcript.BannerInfo, Text: "Stopped listening. The agent may still be running; its output is not shown here until the session is reopened."})
		return nil
	}
	armedAt := time.Now()
	turn.stopArmedAt = armedAt
	return tea.Tick(stopArmWindow, func(time.Time) tea.Msg { return stopDisarmMsg{armedAt: armedAt} })
}

// disarmStop closes the double-tap window; any key but esc does.
func (m *chatModel) disarmStop() {
	if turn, ok := m.turn.(*streamingTurn); ok {
		turn.stopArmedAt = time.Time{}
	}
}

// await parks the chat on a paused task's request; its prompt replaces the composer.
func (m *chatModel) await(pending hitl.Pending) {
	m.stopStream()
	m.working = false
	m.updateStatus()
	m.turn = &awaitingTurn{pending: pending, prompt: newPrompt(m.agentRef, pending)}
	m.layout()
}

// restorePending shows the request a reopened session is holding, unless the
// user has already started a turn.
func (m *chatModel) restorePending(pending hitl.Pending) {
	if _, idle := m.turn.(idleTurn); !idle {
		return
	}
	m.await(pending)
}

// loadHistory shows a reopened session's past tasks and the request one of them holds.
func (m *chatModel) loadHistory(tasks []*protocol.Task, err error) {
	m.historyPending = false
	if err != nil {
		m.appendEntry(transcript.Banner{Kind: transcript.BannerError, Text: fmt.Sprintf("Could not load history: %v", err)})
		return
	}
	for _, task := range tasks {
		m.log.ApplyTask(task)
	}
	m.log.StartTurn()
	if n := len(tasks); n > 0 && tasks[n-1] != nil {
		switch tasks[n-1].Status.State {
		case protocol.TaskStateWorking, protocol.TaskStateSubmitted:
			// Likely a turn whose stream was stopped; its result shows once the session is reopened after it ends.
			m.log.Append(transcript.Banner{Kind: transcript.BannerInfo, Text: "The last turn is still running. Reopen the session later to see its result."})
		}
	}
	m.render()
	if pending, ok := hitl.LastPending(tasks); ok {
		m.restorePending(pending)
	}
}

// awaitKey routes a key to the paused turn: ctrl+x rejects the request (after y),
// anything else goes to the prompt, and a complete answer resumes the task.
func (m *chatModel) awaitKey(turn *awaitingTurn, msg tea.KeyMsg) tea.Cmd {
	if turn.discard == discardConfirming {
		switch msg.String() {
		case "y":
			return m.decline(turn.pending)
		case "n", "esc":
			turn.discard = discardNone
		}
		return nil
	}
	if msg.String() == "ctrl+x" {
		turn.discard = discardConfirming
		return nil
	}
	if !turn.prompt.key(msg) {
		return nil
	}
	message, decision, err := turn.prompt.answer(m.sessionID)
	if err != nil {
		m.appendEntry(transcript.Banner{Kind: transcript.BannerError, Text: fmt.Sprintf("Answer not sent: %v", err)})
		return nil
	}
	m.appendRecord(turn.pending.Request, decision)
	pending := turn.pending
	return m.startStream(message, &pending)
}

// decline rejects the request: the runtimes reject every pending call, and a
// question tool reports that the user declined. Cancelling the task is not
// used because not every runtime supports it.
func (m *chatModel) decline(pending hitl.Pending) tea.Cmd {
	m.appendRecord(pending.Request, hitl.Decision{Type: hitl.Reject})
	return m.startStream(pending.Decline(m.sessionID), &pending)
}

func (m *chatModel) appendRecord(request hitl.Request, decision hitl.Decision) {
	if record, ok := transcript.RecordFor(request, decision); ok {
		m.appendEntry(record)
		return
	}
	m.appendEntry(transcript.Banner{Kind: transcript.BannerInfo, Text: "Rejected the request."})
}

// appendEntry adds an entry after everything shown.
func (m *chatModel) appendEntry(entry transcript.Entry) {
	m.log.Append(entry)
	m.render()
}

// ResetTranscript clears the conversation under a new title.
func (m *chatModel) ResetTranscript(title string) {
	m.title = title
	m.log = transcript.NewLog()
	m.folds = transcript.Folds{}
	m.scrolledBack = false
	m.render()
}

// SetInputVisible toggles input visibility.
func (m *chatModel) SetInputVisible(visible bool) {
	m.showInput = visible
}

// SetFocused moves the cursor into or out of the composer as the pane gains or loses focus.
func (m *chatModel) SetFocused(focused bool) {
	m.focused = focused
	if focused && m.mode == modeCompose {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
}

// render redraws the viewport at its width; entries wrap themselves.
// The viewport follows the newest entry unless the reader has paged up;
// in select mode it follows the cursor instead.
func (m *chatModel) render() {
	entries := m.log.Entries()
	m.selected = min(m.selected, max(len(entries)-1, 0))
	blocks := make([]string, 0, len(entries)+1)
	title := theme.HeadingStyle().Render(m.title)
	blocks = append(blocks, title)
	line := lipgloss.Height(title) + 1
	// start and end are the first and last viewport line of the selected block.
	start, end := 0, 0
	for i, entry := range entries {
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
		if !m.scrolledBack {
			m.vp.GotoBottom()
		}
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
	n := len(m.log.Entries())
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
	m.scrolledBack = false
	m.input.Focus()
	m.render()
}

// selectKey moves over every entry (so the reader can read and copy any block);
// toggling is meaningful for tool activity.
func (m *chatModel) selectKey(msg tea.KeyMsg) tea.Cmd {
	last := len(m.log.Entries()) - 1
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
	case "y":
		m.copySelected()
	case "Y":
		m.copyTranscript()
	case "e":
		return m.exportTranscript()
	case "esc", "ctrl+g", "i":
		m.leaveSelect()
		return nil
	}
	m.render()
	return nil
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
		dur := time.Since(m.workStart).Round(time.Second)
		m.statusText = fmt.Sprintf("Working… %s", dur.String())
	} else {
		m.statusText = ""
	}
}
