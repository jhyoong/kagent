package tui

import (
	"context"
	"time"

	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/hitl"
	"trpc.group/trpc-go/trpc-a2a-go/protocol"
)

// stopArmWindow is how long the first esc waits for the second before disarming.
const stopArmWindow = time.Second

// turnState is the chat's current turn. Each state holds only what is meaningful in it,
// so stream bookkeeping cannot outlive its stream.
type turnState interface{ isTurnState() }

// idleTurn accepts a new message.
type idleTurn struct{}

// streamingTurn is a sent message whose response is still arriving.
type streamingTurn struct {
	// gen tags this turn's stream messages; a message from an earlier stream is stale.
	gen  uint64
	ch   <-chan protocol.StreamingMessageEvent
	stop context.CancelFunc
	// paused is the request the task paused on, once the stream reports it.
	paused *hitl.Pending
	// stopArmedAt is set by the first esc; zero means disarmed.
	stopArmedAt time.Time
}

// awaitingTurn is a paused task; its prompt replaces the composer until the
// user answers or rejects the request, either of which resumes the task.
type awaitingTurn struct {
	pending hitl.Pending
	prompt  prompt
	discard discardStep
}

// discardStep is how far ctrl+x has got: rejecting the request cannot be undone, so it asks first.
type discardStep int

const (
	discardNone discardStep = iota
	discardConfirming
)

func (idleTurn) isTurnState()       {}
func (*streamingTurn) isTurnState() {}
func (*awaitingTurn) isTurnState()  {}

// streamMsg is one event of the stream tagged gen.
type streamMsg struct {
	gen   uint64
	event protocol.StreamingMessageEvent
}

// streamDoneMsg reports that the stream tagged gen closed.
type streamDoneMsg struct{ gen uint64 }

// stopDisarmMsg closes the double-esc window opened at armedAt.
type stopDisarmMsg struct{ armedAt time.Time }
