package tui

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/transcript"
)

// largeCopyBytes is where a copy gets a warning; many terminals and tmux cap OSC 52 payloads near here.
const largeCopyBytes = 75 * 1024

// exportDoneMsg reports that the pager exited; the temporary transcript file is no longer needed.
type exportDoneMsg struct {
	path string
	err  error
}

// copyNote is the status line after a copy attempt.
func copyNote(n int, err error) string {
	switch {
	case err != nil:
		return fmt.Sprintf("copy failed: %v", err)
	case n > largeCopyBytes:
		return fmt.Sprintf("copied %d bytes (large: the terminal may truncate it; use e to export)", n)
	}
	return fmt.Sprintf("copied %d bytes", n)
}

// transcriptText is the whole visible transcript as plain text, entries separated by a blank line.
func transcriptText(entries []transcript.Entry) string {
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = transcript.PlainText(e)
	}
	return strings.Join(parts, "\n\n")
}

// lastAnswer is the text of the most recent agent text entry, without the "Agent:" label so
// it pastes as is. Text between tool calls is a separate entry, so a turn that ended after
// tools yields only its closing text.
func lastAnswer(entries []transcript.Entry) (string, bool) {
	for _, entry := range slices.Backward(entries) {
		if answer, ok := entry.(transcript.AgentText); ok {
			return answer.Text, true
		}
	}
	return "", false
}

// pagerCommand opens path in $PAGER (which may carry arguments), or less.
func pagerCommand(pager, path string) *exec.Cmd {
	fields := strings.Fields(pager)
	if len(fields) == 0 {
		fields = []string{"less"}
	}
	return exec.Command(fields[0], append(fields[1:], path)...)
}

// copyText writes text to the clipboard and sets the status note.
func (m *chatModel) copyText(text string) {
	if m.clip == nil {
		m.note = "clipboard unavailable"
		return
	}
	m.note = copyNote(len(text), m.clip.WriteClipboard(text))
}

// copyLastAnswer is ctrl+y.
func (m *chatModel) copyLastAnswer() {
	text, ok := lastAnswer(m.log.Entries())
	if !ok {
		m.note = "no agent answer to copy"
		return
	}
	m.copyText(text)
}

// copySelected is y in select mode.
func (m *chatModel) copySelected() {
	visible := m.log.Entries()
	if m.selected < 0 || m.selected >= len(visible) {
		m.note = "nothing to copy"
		return
	}
	m.copyText(transcript.PlainText(visible[m.selected]))
}

// copyTranscript is Y in select mode.
func (m *chatModel) copyTranscript() {
	visible := m.log.Entries()
	if len(visible) == 0 {
		m.note = "nothing to copy"
		return
	}
	m.copyText(transcriptText(visible))
}

// exportTranscript writes the transcript to a temp file and opens it in the pager.
func (m *chatModel) exportTranscript() tea.Cmd {
	visible := m.log.Entries()
	if len(visible) == 0 {
		m.note = "nothing to export"
		return nil
	}
	f, err := os.CreateTemp("", "kagent-transcript-*.txt")
	if err != nil {
		m.note = fmt.Sprintf("export failed: %v", err)
		return nil
	}
	path := f.Name()
	_, werr := f.WriteString(transcriptText(visible) + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
		m.note = fmt.Sprintf("export failed: %v", werr)
		return nil
	}
	return tea.Exec(&pagerExec{pagerCommand(os.Getenv("PAGER"), path)}, func(err error) tea.Msg {
		return exportDoneMsg{path: path, err: err}
	})
}

// pagerExec runs the pager on the process's real stdout. tea.ExecProcess would hand the
// command the program's output writer, which is not an *os.File, so exec would pipe it and
// the pager would see no terminal.
type pagerExec struct{ cmd *exec.Cmd }

func (p *pagerExec) SetStdin(r io.Reader) { p.cmd.Stdin = r }
func (p *pagerExec) SetStdout(io.Writer)  { p.cmd.Stdout = os.Stdout }
func (p *pagerExec) SetStderr(io.Writer)  { p.cmd.Stderr = os.Stderr }
func (p *pagerExec) Run() error           { return p.cmd.Run() }

// applyExportDone removes the temp file.
func (m *chatModel) applyExportDone(msg exportDoneMsg) {
	_ = os.Remove(msg.path)
	if msg.err != nil {
		m.note = fmt.Sprintf("pager failed: %v", msg.err)
	}
}
