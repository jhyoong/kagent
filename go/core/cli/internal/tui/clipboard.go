package tui

import (
	"io"
	"os"
	"sync"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// clipboardWriter puts text on the user's system clipboard.
type clipboardWriter interface {
	WriteClipboard(text string) error
}

// lockedOutput is the program's terminal output. Bubble Tea v1 has no clipboard command,
// and its renderer writes frames from its own goroutine. Sending an OSC 52 sequence
// straight to the TTY could land in the middle of a frame write and garble it.
// Passing this writer to tea.WithOutput makes the renderer's frames and our sequence
// serialize on one mutex, so each is written whole.
//
// Bubble Tea only treats the output as a terminal (window size, restoring terminal state)
// when it implements term.File: Read, Write, Close and Fd. All four are provided.
type lockedOutput struct {
	mu sync.Mutex
	f  *os.File
}

func newLockedOutput(f *os.File) *lockedOutput { return &lockedOutput{f: f} }

func (o *lockedOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.f.Write(p)
}

func (o *lockedOutput) Read(p []byte) (int, error) { return o.f.Read(p) }

func (o *lockedOutput) Fd() uintptr { return o.f.Fd() }

// Close does nothing: stdout belongs to the process, not to this writer, and Bubble Tea
// must not be able to close it through the wrapper.
func (o *lockedOutput) Close() error { return nil }

var _ term.File = (*lockedOutput)(nil)

// WriteClipboard sends an OSC 52 "set system clipboard" sequence. Terminals that do not
// support it ignore it, so success means written, not applied.
func (o *lockedOutput) WriteClipboard(text string) error {
	_, err := io.WriteString(o, ansi.SetSystemClipboard(text))
	return err
}
