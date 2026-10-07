package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	clia2a "github.com/kagent-dev/kagent/go/core/cli/internal/a2a"
	"github.com/kagent-dev/kagent/go/core/cli/internal/tui/hitl"
)

// cancelArmWindow is how long the first esc waits for the second before disarming.
const cancelArmWindow = time.Second

// turnClient is what a chat needs from its session's A2A client.
// The client opts in to the HITL extension on every call, CancelTask included.
type turnClient interface {
	SendStreamingMessage(ctx context.Context, req *a2atype.SendMessageRequest) iter.Seq2[a2atype.Event, error]
	CancelTask(ctx context.Context, req *a2atype.CancelTaskRequest) (*a2atype.Task, error)
	GetTask(ctx context.Context, req *a2atype.GetTaskRequest) (*a2atype.Task, error)
}

// turnState is the chat's current turn. Each state holds only what is meaningful in it,
// so stream bookkeeping cannot outlive its stream.
type turnState interface{ isTurnState() }

// idleTurn accepts a new message.
type idleTurn struct{}

// streamingTurn is a sent message whose response is still arriving.
type streamingTurn struct {
	// gen tags this turn's stream messages; a message from an earlier stream is stale.
	gen  uint64
	ch   <-chan clia2a.StreamResult
	stop context.CancelFunc
	// assembler reduces the stream; projected is its last text projection, so cumulative
	// chunks yield a delta, not a duplicate.
	assembler *clia2a.Assembler
	projected string
	lastState a2atype.TaskState
	// resumed is the paused task this stream answers; nil for a new message. A replay of
	// its pause is not a new pause, and a failed stream must not strand its request.
	resumed *a2atype.Task

	// cancelArmedAt is set by the first esc; zero means disarmed.
	cancelArmedAt time.Time
	// cancel is how far a requested cancel has got.
	cancel cancelPhase
}

// cancelPhase is a turn's cancel: none, waiting for the first event that names the task,
// or a CancelTask call in flight while the stream stays open for the canceled status.
type cancelPhase int

const (
	cancelNone cancelPhase = iota
	cancelRequested
	cancelInFlight
)

// awaitingTurn is a paused task; its prompt replaces the composer until the
// user answers (resuming the task) or discards the request (cancelling it).
type awaitingTurn struct {
	// paused seeds the resumed stream, so output from before the pause is not repeated.
	paused  *a2atype.Task
	pending hitl.Pending
	prompt  prompt
	discard discardStep
}

// discardStep is how far ctrl+x has got: discarding cancels the task, so it asks first.
type discardStep int

const (
	discardNone discardStep = iota
	discardConfirming
	discardSending
)

func (idleTurn) isTurnState()       {}
func (*streamingTurn) isTurnState() {}
func (*awaitingTurn) isTurnState()  {}

// taskID is the task this turn's stream belongs to, or "" before the first task event.
func (t *streamingTurn) taskID() a2atype.TaskID {
	if task, ok := t.assembler.Result().(*a2atype.Task); ok {
		return task.ID
	}
	return ""
}

// streamMsg is one result of the stream tagged gen; streamDoneMsg says that stream ended.
type streamMsg struct {
	gen    uint64
	result clia2a.StreamResult
}

type streamDoneMsg struct{ gen uint64 }

// cancelDisarmMsg ends the double-tap window opened at armedAt; a later arm ignores it.
type cancelDisarmMsg struct{ armedAt time.Time }

// cancelResultMsg reports a CancelTask call. It names its session and the stream it was
// issued for, so it cannot land in another chat or settle a later turn.
type cancelResultMsg struct {
	contextID string
	gen       uint64
	taskID    a2atype.TaskID
	err       error
}

// pauseCheckedMsg reports where a task stands after the stream answering it failed.
type pauseCheckedMsg struct {
	contextID string
	// paused is the request the failed stream answered.
	paused *a2atype.Task
	task   *a2atype.Task
	err    error
}

// discardResultMsg reports the CancelTask that discards a pending request, with the task it left.
type discardResultMsg struct {
	contextID string
	taskID    a2atype.TaskID
	task      *a2atype.Task
	err       error
}

// pausedAgain reports whether task, seen by a resumed stream, holds a new request
// rather than a replay of the one answered. The state alone cannot tell: both are
// input-required, possibly with nothing in between.
func (t *streamingTurn) pausedAgain(task *a2atype.Task) bool {
	if t.resumed == nil || task.Status.State != a2atype.TaskStateInputRequired {
		return false
	}
	return !samePause(task.Status.Message, t.resumed.Status.Message)
}

// samePause reports whether two status messages are the same request: by message
// id when both have one, otherwise by content.
func samePause(a, b *a2atype.Message) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.ID != "" && b.ID != "" {
		return a.ID == b.ID
	}
	aJSON, aErr := json.Marshal(a)
	bJSON, bErr := json.Marshal(b)
	return aErr == nil && bErr == nil && bytes.Equal(aJSON, bJSON)
}
