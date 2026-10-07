package tui

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/transcript"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeClipboard struct {
	writes []string
	err    error
}

func (f *fakeClipboard) WriteClipboard(text string) error {
	f.writes = append(f.writes, text)
	return f.err
}

func copyChat(t *testing.T) (*workspaceModel, *fakeClipboard) {
	t.Helper()
	m := openedChat(t)
	clip := &fakeClipboard{}
	m.chat.clip = clip
	return m, clip
}

func TestCtrlYWithoutAnswerShowsAStatus(t *testing.T) {
	m, clip := copyChat(t)
	m.chat.appendUser("hello")

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})

	assert.Empty(t, clip.writes)
	assert.Contains(t, m.chat.View(), "no agent answer to copy")
}

func TestCtrlYCopiesTheLastAgentAnswer(t *testing.T) {
	m, clip := copyChat(t)
	m.chat.appendEntry(transcript.AgentText{Text: "first"})
	m.chat.appendUser("and?")
	m.chat.appendEntry(transcript.AgentText{Text: "the final answer"})

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})

	assert.Equal(t, []string{"the final answer"}, clip.writes, "no Agent: label")
	assert.Contains(t, m.chat.View(), "copied 16 bytes")

	m.Update(runes("x"))
	assert.NotContains(t, m.chat.View(), "copied", "the note clears on the next key")
}

func TestCtrlYReportsAWriteFailureAndLargeCopies(t *testing.T) {
	m, clip := copyChat(t)
	clip.err = assert.AnError
	m.chat.appendEntry(transcript.AgentText{Text: "x"})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
	assert.Contains(t, m.chat.note, "copy failed")

	assert.Contains(t, copyNote(largeCopyBytes+1, nil), "large")
	assert.NotContains(t, copyNote(largeCopyBytes, nil), "large")
}

func TestCopyWithoutAClipboardSaysSo(t *testing.T) {
	m := openedChat(t)
	m.chat.appendEntry(transcript.AgentText{Text: "x"})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
	assert.Equal(t, "clipboard unavailable", m.chat.note)
}

func TestSelectModeCopiesTheEntryAndTheTranscript(t *testing.T) {
	m, clip := copyChat(t)
	m.chat.appendUser("why?")
	m.chat.appendEntry(transcript.AgentText{Text: "because"})

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	require.Equal(t, modeSelect, m.chat.mode)

	m.Update(runes("y"))
	require.Len(t, clip.writes, 1)
	assert.Equal(t, transcript.PlainText(transcript.AgentText{Text: "because"}), clip.writes[0])

	m.Update(runes("Y"))
	require.Len(t, clip.writes, 2)
	want := transcript.PlainText(transcript.UserMessage{Text: "why?"}) + "\n\n" + clip.writes[0]
	assert.Equal(t, want, clip.writes[1])
	assert.NotContains(t, clip.writes[1], "\x1b", "no ANSI in copied text")
	assert.Equal(t, modeSelect, m.chat.mode)
}

func TestSelectModeExportWritesATempFileAndOpensThePager(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	m, _ := copyChat(t)
	m.chat.appendEntry(transcript.AgentText{Text: "export me"})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})

	_, cmd := m.Update(runes("e"))
	require.NotNil(t, cmd)

	files, err := filepath.Glob(filepath.Join(tmp, "kagent-transcript-*.txt"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	body, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Contains(t, string(body), "export me")

	_, cmd = m.Update(exportDoneMsg{path: files[0]})
	require.NotNil(t, cmd)
	assert.Equal(t, tea.EnableMouseCellMotion(), cmd(), "the pager exit re-enables the mouse")
	_, err = os.Stat(files[0])
	assert.True(t, os.IsNotExist(err), "the temp file is removed once the pager exits")
}

func TestPagerCommand(t *testing.T) {
	assert.Equal(t, []string{"less", "/t"}, pagerCommand("", "/t").Args)
	assert.Equal(t, []string{"less", "-R", "/t"}, pagerCommand("less -R", "/t").Args)
}

func TestSelectLayoutHidesChromeAndReleasesTheMouse(t *testing.T) {
	m := openedChat(t)
	m.showDetails = true
	m.chat.appendUser("why is checkout failing?")
	normal := m.View()
	require.Contains(t, normal, "╭")

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	require.NotNil(t, cmd)
	assert.Equal(t, tea.DisableMouse(), cmd(), "mouse capture is released")

	view := ansi.Strip(m.View())
	assert.NotContains(t, view, "╭", "no borders")
	assert.NotContains(t, view, "Namespaces")
	assert.Contains(t, view, "select layout (mouse released) · ctrl+l to return")
	assert.Contains(t, view, "why is checkout failing?")
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "why is checkout failing?") {
			assert.True(t, strings.HasPrefix(line, "You:"), "text starts at column 0: %q", line)
		}
	}

	focus := m.focus
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	assert.Equal(t, focus, m.focus, "tab is disabled")
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	assert.True(t, m.showDetails)

	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	require.NotNil(t, cmd)
	assert.Equal(t, tea.EnableMouseCellMotion(), cmd(), "mouse capture is restored")
	assert.Contains(t, m.View(), "╭")
}

func TestLockedOutputWritesAWholeOSC52Sequence(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "out")
	require.NoError(t, err)
	out := newLockedOutput(f)

	require.NoError(t, out.WriteClipboard("hi"))

	got, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	assert.Equal(t, ansi.SetSystemClipboard("hi"), string(got))
}

func TestExportDoneKeepsTheMouseReleasedInSelectLayout(t *testing.T) {
	m, _ := copyChat(t)
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	_, cmd := m.Update(exportDoneMsg{path: filepath.Join(t.TempDir(), "gone")})
	assert.Nil(t, cmd)
}

// Bubble Tea only sets up window-size handling when the output is a term.File with a terminal Fd.
func TestLockedOutputIsATerminalFileToBubbleTea(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "out")
	require.NoError(t, err)
	out := newLockedOutput(f)

	var w io.Writer = out
	file, ok := w.(term.File)
	require.True(t, ok)
	assert.Equal(t, f.Fd(), file.Fd())
	assert.NoError(t, out.Close())
	_, err = f.Write([]byte("x"))
	assert.NoError(t, err, "Close does not close the wrapped file")
}
