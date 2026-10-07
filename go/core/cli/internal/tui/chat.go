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
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/hitl"
	sessionview "github.com/kagent-dev/kagent/go/core/cli/internal/tui/session"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/theme"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/transcript"
)

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
	// height is the whole chat's; the viewport gets what the header and bottom block leave.
	height int

	// entries is the conversation. When textOpen, the last entry is the AgentText still being
	// assembled; appending anything else closes it.
	entries  []transcript.Entry
	textOpen bool
	// turnStart is where the current turn's entries begin. Call IDs repeat across turns, so
	// tool activity pairs and Visible filters only within a turn; earlier entries are sealed.
	turnStart int

	// folds is indexed by position in visibleEntries(). When Visible newly hides an earlier
	// entry of the current turn, render moves folds and selected with their entries.
	folds transcript.Folds
	// shown is the entries index of each visible position at the last render.
	shown []int
	mode  chatMode
	// selected is the visibleEntries() index under the cursor; meaningful only in modeSelect.
	selected int

	working    bool
	workStart  time.Time
	statusText string

	spin spinner.Model

	// ctx is the workspace's context, so cancelling the program cancels an in-flight stream.
	ctx    context.Context
	client turnClient
	// turn is never nil: idleTurn, *streamingTurn or *awaitingTurn.
	turn turnState
	// historyPending is true from opening a session until its history, and any
	// request it holds, has arrived; a send before then could race restorePending.
	historyPending bool
	// streamGen numbers streams, so a message from a replaced stream is recognised as stale.
	streamGen uint64
}

func newChatModel(ctx context.Context, agentRef string, contextID string, client turnClient, verbose bool) *chatModel {
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
		client:    client,
		turn:      idleTurn{},
		spin:      sp,
	}
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
			return tea.Batch(cmds...)
		}
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

		// Re-render content if width changed
		if oldWidth != msg.Width && msg.Width > 0 {
			m.render()
		}
		return nil
	case tea.KeyMsg:
		if msg.Type != tea.KeyEsc {
			m.disarmCancel()
		}
		if msg.String() == "ctrl+o" {
			m.toggleAll()
			return nil
		}
		if m.mode == modeSelect {
			m.selectKey(msg)
			return nil
		}
		if msg.String() == "ctrl+g" {
			m.enterSelect()
			return nil
		}
		if turn, ok := m.turn.(*awaitingTurn); ok {
			return m.awaitKey(turn, msg)
		}
		switch msg.String() {
		case "esc":
			// Never quits; the workspace owns ctrl+c. Twice in a row cancels the running turn.
			return m.pressEsc()
		case "enter":
			if m.isStreaming() {
				return nil
			}
			if m.historyPending {
				return nil
			}
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return nil
			}
			m.appendUser(text)
			m.input.Reset()
			return m.submit(text)
		}
	case streamMsg:
		if !m.isCurrentStream(msg.gen) {
			return nil // the stream this belonged to has ended or been replaced
		}
		if msg.result.Err != nil {
			resumed := m.turn.(*streamingTurn).resumed
			m.appendTransportError(msg.result.Err)
			m.endStream()
			if resumed != nil {
				// The answer may not have arrived, so the request may still be waiting.
				return m.checkPause(resumed)
			}
			return nil
		}
		m.appendEvent(msg.result.Event)
		return tea.Batch(m.waitNext(), m.fireDeferredCancel())
	case streamDoneMsg:
		if m.isCurrentStream(msg.gen) {
			m.endStream()
		}
		return nil
	case cancelDisarmMsg:
		if turn, ok := m.turn.(*streamingTurn); ok && turn.cancelArmedAt.Equal(msg.armedAt) {
			turn.cancelArmedAt = time.Time{}
		}
		return nil
	case cancelResultMsg:
		m.applyCancelResult(msg)
		return nil
	case discardResultMsg:
		m.applyDiscardResult(msg)
		return nil
	case pauseCheckedMsg:
		m.applyPauseCheck(msg)
		return nil
	}

	if _, awaiting := m.turn.(*awaitingTurn); !awaiting {
		m.input, cmd = m.input.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
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
	if turn, ok := m.turn.(*streamingTurn); ok {
		switch {
		case turn.cancel != cancelNone:
			status += "  Canceling…"
		case !turn.cancelArmedAt.IsZero():
			status += "  press esc again to cancel"
		}
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
		m.bottomView(width),
	)
}

// bottomView is the composer, or the prompt of a paused turn with its hint line.
func (m *chatModel) bottomView(width int) string {
	turn, ok := m.turn.(*awaitingTurn)
	if !ok {
		return m.input.View()
	}
	hints := turn.prompt.hints()
	switch turn.discard {
	case discardConfirming:
		hints = "Discard this request? The task is canceled.  y discard · n keep"
	case discardSending:
		hints = "Discarding…"
	}
	return turn.prompt.view(width) + "\n" + wrapLines([]string{theme.DimStyle().Render(hints)}, width)
}

// layout gives the viewport the height the header, status and bottom block leave,
// measured because a prompt is taller than the composer and changes as it is used.
func (m *chatModel) layout() {
	if m.height <= 0 {
		return
	}
	width := m.vp.Width
	// Two rules and the status line sit between the header and the bottom block.
	chrome := lipgloss.Height(m.headerView(width)) + 3 + lipgloss.Height(m.bottomView(width))
	if height := max(m.height-chrome, 5); height != m.vp.Height {
		m.vp.Height = height
		m.render()
	}
}

// setHeaderMeta adds lifecycle detail to the header; the caller pre-renders state to keep this type-free.
func (m *chatModel) setHeaderMeta(state string, lastActive time.Time) {
	m.state, m.lastActive = state, lastActive
}

// stop cancels an in-flight stream, so a replaced chat stops delivering.
// It drops the connection only; the task itself runs on (esc esc cancels it).
func (m *chatModel) stop() {
	if m == nil {
		return
	}
	if turn, ok := m.turn.(*streamingTurn); ok && turn.stop != nil {
		turn.stop()
	}
	m.turn = idleTurn{}
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
	return m.startStream(a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart(text)))
}

// startStream sends msg in this chat's session and makes it the running turn.
// The caller shows msg in the transcript; this only seals the previous turn.
func (m *chatModel) startStream(msg *a2atype.Message) tea.Cmd {
	m.stop()
	m.sealTurn()
	return m.openStream(msg, &streamingTurn{assembler: &clia2a.Assembler{}})
}

// resume answers the paused task. The turn continues rather than starting over:
// resumed results settle the paused turn's tool entries, and the assembler and
// projection start from the paused task so its output is not shown again.
func (m *chatModel) resume(msg *a2atype.Message, paused *a2atype.Task) tea.Cmd {
	projected, err := assembledText(paused)
	if err != nil {
		m.appendTransportError(err)
	}
	return m.openStream(msg, &streamingTurn{
		assembler: clia2a.ResumeAssembler(paused),
		projected: projected,
		// A replayed snapshot of the pause is not a new pause; see renderState.
		lastState: a2atype.TaskStateInputRequired,
		resumed:   paused,
	})
}

// openStream sends msg and makes turn, seeded by the caller, the running turn.
func (m *chatModel) openStream(msg *a2atype.Message, turn *streamingTurn) tea.Cmd {
	m.stop()
	m.setWorkingTime(time.Time{})
	msg.ContextID = m.contextID
	ctx, stop := context.WithCancel(m.ctx)
	stream := m.client.SendStreamingMessage(ctx, &a2atype.SendMessageRequest{Message: msg})
	m.streamGen++
	turn.gen, turn.ch, turn.stop = m.streamGen, clia2a.StreamToChannel(ctx, stream), stop
	m.turn = turn
	return tea.Batch(m.waitNext(), m.tick())
}

func (m *chatModel) waitNext() tea.Cmd {
	turn, ok := m.turn.(*streamingTurn)
	if !ok {
		return nil
	}
	ch, gen := turn.ch, turn.gen
	return func() tea.Msg {
		result, ok := <-ch
		if !ok {
			return streamDoneMsg{gen: gen}
		}
		return streamMsg{gen: gen, result: result}
	}
}

// endStream commits any in-flight agent text and clears the working state.
func (m *chatModel) endStream() {
	m.stop()
	m.textOpen = false
	m.working = false
	m.lastActive = time.Now()
	m.updateStatus()
}

// appendEvent reduces one event of the running turn and renders what it changed.
func (m *chatModel) appendEvent(ev a2atype.Event) {
	turn, ok := m.turn.(*streamingTurn)
	if !ok || ev == nil {
		return
	}
	if err := turn.assembler.Apply(ev); err != nil {
		m.appendEntry(transcript.Banner{Kind: transcript.BannerError, Text: fmt.Sprintf("Protocol error: %v", err)})
		return
	}
	m.renderToolActivity(eventParts(ev))
	m.renderAssembledText(turn)
	m.renderState(turn)
}

// pressEsc arms the cancel on the first esc of a running turn and cancels on the second.
// A cancel already requested or in flight is not repeated.
func (m *chatModel) pressEsc() tea.Cmd {
	turn, ok := m.turn.(*streamingTurn)
	if !ok || turn.cancel != cancelNone {
		return nil
	}
	if !turn.cancelArmedAt.IsZero() && time.Since(turn.cancelArmedAt) <= cancelArmWindow {
		turn.cancelArmedAt = time.Time{}
		turn.cancel = cancelRequested
		return m.fireDeferredCancel()
	}
	armedAt := time.Now()
	turn.cancelArmedAt = armedAt
	return tea.Tick(cancelArmWindow, func(time.Time) tea.Msg { return cancelDisarmMsg{armedAt: armedAt} })
}

// disarmCancel closes the double-tap window; any key but esc does.
func (m *chatModel) disarmCancel() {
	if turn, ok := m.turn.(*streamingTurn); ok {
		turn.cancelArmedAt = time.Time{}
	}
}

// fireDeferredCancel issues a requested cancel once the stream has named its task.
func (m *chatModel) fireDeferredCancel() tea.Cmd {
	turn, ok := m.turn.(*streamingTurn)
	if !ok || turn.cancel != cancelRequested {
		return nil
	}
	id := turn.taskID()
	if id == "" {
		return nil
	}
	turn.cancel = cancelInFlight
	client, ctx, contextID, gen := m.client, m.ctx, m.contextID, turn.gen
	return func() tea.Msg {
		_, err := client.CancelTask(ctx, &a2atype.CancelTaskRequest{ID: id})
		return cancelResultMsg{contextID: contextID, gen: gen, taskID: id, err: err}
	}
}

// applyCancelResult reports a failed cancel and lets esc esc retry it. Success on the
// stream it was issued for needs nothing: the turn stays "Canceling…" until the stream
// delivers the canceled status and ends. A result for an earlier stream only reports
// failure and never changes the current turn; a success that lands after the turn
// paused closes the prompt of the task it canceled.
func (m *chatModel) applyCancelResult(msg cancelResultMsg) {
	if msg.contextID != m.contextID {
		return
	}
	turn, streaming := m.turn.(*streamingTurn)
	current := streaming && turn.gen == msg.gen
	if msg.err != nil {
		if current {
			turn.cancel = cancelNone
		}
		m.appendEntry(transcript.Banner{Kind: transcript.BannerError, Text: fmt.Sprintf("Cancel failed: %v", msg.err)})
		return
	}
	if awaiting, ok := m.turn.(*awaitingTurn); ok && awaiting.pending.TaskID == msg.taskID {
		m.turn = idleTurn{}
		m.appendEntry(transcript.Banner{Kind: transcript.BannerInfo, Text: "Task canceled."})
	}
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
func (m *chatModel) renderAssembledText(turn *streamingTurn) {
	text, err := assembledText(turn.assembler.Result())
	if err != nil {
		m.appendTransportError(err)
		return
	}
	if text == turn.projected {
		return
	}
	delta, extends := strings.CutPrefix(text, turn.projected)
	turn.projected = text
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
		// A new block does not start with the separator joining it to the text before.
		text = strings.TrimLeft(delta, "\n")
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
func (m *chatModel) renderState(turn *streamingTurn) {
	task, ok := turn.assembler.Result().(*a2atype.Task)
	if !ok {
		return
	}
	state := task.Status.State
	if state == turn.lastState && !turn.pausedAgain(task) {
		return
	}
	turn.lastState = state

	switch state {
	case a2atype.TaskStateAuthRequired:
		m.appendEntry(transcript.Banner{Kind: transcript.BannerInfo, Text: "⏸ Authentication required. This task cannot continue here."})
	case a2atype.TaskStateFailed, a2atype.TaskStateRejected, a2atype.TaskStateCanceled:
		banner := fmt.Sprintf("Task %s.", stateLabel(state))
		kind := transcript.BannerError
		if state == a2atype.TaskStateCanceled {
			kind = transcript.BannerInfo // the user asked for it
		} else {
			banner = "✗ " + banner
		}
		if task.Status.Message != nil {
			detail, err := clia2a.PartsText(task.Status.Message.Parts)
			if err != nil {
				m.appendTransportError(err)
			} else if strings.TrimSpace(detail) != "" {
				banner += " " + detail
			}
		}
		m.appendEntry(transcript.Banner{Kind: kind, Text: banner})
	}
	if state.Terminal() || state == a2atype.TaskStateInputRequired || state == a2atype.TaskStateAuthRequired {
		m.working = false
		m.updateStatus()
	} else if task.Status.Timestamp != nil {
		m.setWorkingTime(*task.Status.Timestamp)
	}
	if state == a2atype.TaskStateInputRequired {
		// The pause is final for this stream; its prompt takes over from the composer.
		if turn.cancel == cancelRequested {
			m.appendEntry(transcript.Banner{Kind: transcript.BannerInfo, Text: "Cancel not sent: the agent is waiting for input. Answer it, or press ctrl+x to discard the request."})
		}
		m.await(task)
	}
}

// stateLabel is a task state in words: "canceled", "input required".
func stateLabel(state a2atype.TaskState) string {
	label := strings.TrimPrefix(string(state), "TASK_STATE_")
	return strings.ToLower(strings.ReplaceAll(label, "_", " "))
}

// await parks the chat on task's request. Its stream, if any, is over.
func (m *chatModel) await(task *a2atype.Task) {
	pending, ok := hitl.ReadPending(task)
	if !ok {
		return
	}
	m.stop()
	m.textOpen = false
	m.working = false
	m.lastActive = time.Now()
	m.updateStatus()
	m.turn = &awaitingTurn{paused: task, pending: pending, prompt: newPrompt(m.agentRef, pending)}
	m.layout()
}

// restorePending shows the request a reopened session is holding. The paused
// task becomes the current turn, so its resumed results settle its entries.
// A turn the user already started wins: the session can hold one task only.
func (m *chatModel) restorePending(task *a2atype.Task) {
	if _, idle := m.turn.(idleTurn); !idle || task == nil {
		return
	}
	m.sealTurn()
	m.entries = append(m.entries, transcript.ProjectTask(task)...)
	m.render()
	m.await(task)
}

// awaitKey routes a key to the paused turn: ctrl+x discards (after y), anything
// else goes to the prompt, and a complete answer resumes the task.
func (m *chatModel) awaitKey(turn *awaitingTurn, msg tea.KeyMsg) tea.Cmd {
	switch turn.discard {
	case discardSending:
		return nil
	case discardConfirming:
		switch msg.String() {
		case "y":
			turn.discard = discardSending
			return m.discardRequest(turn.pending.TaskID)
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
	message, record, err := turn.prompt.answer(m.contextID)
	if err != nil {
		m.appendEntry(transcript.Banner{Kind: transcript.BannerError, Text: fmt.Sprintf("Answer not sent: %v", err)})
		return nil
	}
	m.appendEntry(record)
	return m.resume(message, turn.paused)
}

// checkPause reads the task a failed resume answered, to learn whether its request still waits.
func (m *chatModel) checkPause(paused *a2atype.Task) tea.Cmd {
	client, ctx, contextID := m.client, m.ctx, m.contextID
	return func() tea.Msg {
		task, err := client.GetTask(ctx, &a2atype.GetTaskRequest{ID: paused.ID})
		return pauseCheckedMsg{contextID: contextID, paused: paused, task: task, err: err}
	}
}

// applyPauseCheck shows the request again when the task still waits, so it can
// be answered or discarded. If the task cannot be read, the answered request is
// shown: the server most likely still holds it, and if not, answering it again
// fails visibly rather than leaving a request no one can reach.
func (m *chatModel) applyPauseCheck(msg pauseCheckedMsg) {
	if _, idle := m.turn.(idleTurn); msg.contextID != m.contextID || !idle {
		return // the user has moved on; the session holds one task at a time
	}
	switch {
	case msg.err != nil || msg.task == nil:
		reason := "the server returned no task"
		if msg.err != nil {
			reason = msg.err.Error()
		}
		m.appendEntry(transcript.Banner{Kind: transcript.BannerError, Text: fmt.Sprintf("Could not check the task (%s); its request is shown again.", reason)})
		m.await(msg.paused)
	case msg.task.Status.State == a2atype.TaskStateInputRequired:
		m.appendEntry(transcript.Banner{Kind: transcript.BannerInfo, Text: "The task is still waiting for input."})
		m.await(msg.task)
	default:
		m.appendEntry(transcript.Banner{Kind: transcript.BannerInfo, Text: fmt.Sprintf("The task is no longer waiting for input; it is %s.", stateLabel(msg.task.Status.State))})
	}
}

// discardRequest cancels the paused task, the only way to give up its request.
func (m *chatModel) discardRequest(id a2atype.TaskID) tea.Cmd {
	client, ctx, contextID := m.client, m.ctx, m.contextID
	return func() tea.Msg {
		task, err := client.CancelTask(ctx, &a2atype.CancelTaskRequest{ID: id})
		return discardResultMsg{contextID: contextID, taskID: id, task: task, err: err}
	}
}

// applyDiscardResult trusts the task CancelTask returns rather than assuming
// the cancel worked: a task still waiting shows its request again.
func (m *chatModel) applyDiscardResult(msg discardResultMsg) {
	turn, ok := m.turn.(*awaitingTurn)
	if msg.contextID != m.contextID || !ok || turn.pending.TaskID != msg.taskID {
		return
	}
	switch {
	case msg.err != nil:
		turn.discard = discardNone
		m.appendEntry(transcript.Banner{Kind: transcript.BannerError, Text: fmt.Sprintf("Discard failed: %v", msg.err)})
	case msg.task == nil:
		turn.discard = discardNone
		m.appendEntry(transcript.Banner{Kind: transcript.BannerError, Text: "Discard failed: the server returned no task."})
	case msg.task.Status.State.Terminal():
		m.turn = idleTurn{}
		m.appendEntry(transcript.Banner{Kind: transcript.BannerInfo, Text: "Discarded the request; the task is canceled."})
	case msg.task.Status.State == a2atype.TaskStateInputRequired:
		m.appendEntry(transcript.Banner{Kind: transcript.BannerInfo, Text: "The task is still waiting for input."})
		m.await(msg.task)
	default:
		m.turn = idleTurn{}
		m.appendEntry(transcript.Banner{Kind: transcript.BannerError, Text: fmt.Sprintf("The request was not discarded; the task is %s.", stateLabel(msg.task.Status.State))})
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
// Compacting renumbers entries but keeps every visible position.
func (m *chatModel) sealTurn() {
	m.textOpen = false
	m.follow(m.visibleIndices())
	m.entries = append(m.entries[:m.turnStart], transcript.Visible(m.entries[m.turnStart:])...)
	m.turnStart = len(m.entries)
	m.shown = m.visibleIndices()
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

// visibleIndices is visibleEntries as indices into entries.
func (m *chatModel) visibleIndices() []int {
	indices := make([]int, 0, len(m.entries))
	for i := range m.turnStart {
		indices = append(indices, i)
	}
	for _, i := range transcript.VisibleIndices(m.entries[m.turnStart:]) {
		indices = append(indices, m.turnStart+i)
	}
	return indices
}

// follow keeps folds and the select cursor on their entries when the visible
// list changes other than by appending, e.g. a live record hiding a refusal.
func (m *chatModel) follow(now []int) {
	before := m.shown
	m.shown = now
	if len(before) <= len(now) && slices.Equal(before, now[:len(before)]) {
		return
	}
	position := make(map[int]int, len(now))
	for p, entry := range now {
		position[entry] = p
	}
	moved := func(old int) (int, bool) {
		if old < 0 || old >= len(before) {
			return 0, false
		}
		p, ok := position[before[old]]
		return p, ok
	}
	m.folds.Remap(moved)
	if p, ok := moved(m.selected); ok {
		m.selected = p
	}
}

// render redraws the viewport at its width; entries wrap themselves.
// In select mode the viewport follows the cursor instead of the newest entry.
func (m *chatModel) render() {
	m.follow(m.visibleIndices())
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
