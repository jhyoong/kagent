package a2a

import (
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssemblerAppliesArtifactReplacement(t *testing.T) {
	const (
		taskID     = a2atype.TaskID("task-1")
		contextID  = "session-1"
		artifactID = a2atype.ArtifactID("answer")
	)
	events := []a2atype.Event{
		&a2atype.Task{ID: taskID, ContextID: contextID, Status: a2atype.TaskStatus{State: a2atype.TaskStateWorking}},
		&a2atype.TaskArtifactUpdateEvent{
			TaskID: taskID, ContextID: contextID,
			Artifact: &a2atype.Artifact{ID: artifactID, Parts: a2atype.ContentParts{a2atype.NewTextPart("Hel")}},
		},
		&a2atype.TaskArtifactUpdateEvent{
			TaskID: taskID, ContextID: contextID, Append: true,
			Artifact: &a2atype.Artifact{ID: artifactID, Parts: a2atype.ContentParts{a2atype.NewTextPart("lo")}},
		},
		&a2atype.TaskArtifactUpdateEvent{
			TaskID: taskID, ContextID: contextID, LastChunk: true,
			Artifact: &a2atype.Artifact{ID: artifactID, Parts: a2atype.ContentParts{a2atype.NewTextPart("Hello")}},
		},
		&a2atype.TaskStatusUpdateEvent{
			TaskID: taskID, ContextID: contextID,
			Status: a2atype.TaskStatus{State: a2atype.TaskStateCompleted},
		},
	}

	assembler := &Assembler{}
	for _, event := range events {
		require.NoError(t, assembler.Apply(event))
	}

	result, ok := assembler.Result().(*a2atype.Task)
	require.True(t, ok)
	require.Len(t, result.Artifacts, 1)
	require.Len(t, result.Artifacts[0].Parts, 1)
	assert.Equal(t, "Hello", result.Artifacts[0].Parts[0].Text())
	assert.Equal(t, a2atype.TaskStateCompleted, result.Status.State)
	assert.True(t, assembler.Complete())
}

func TestAssemblerAcceptsMessageResult(t *testing.T) {
	message := a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("hello"))
	assembler := &Assembler{}

	require.NoError(t, assembler.Apply(message))
	assert.Same(t, message, assembler.Result())
	assert.True(t, assembler.Complete())
}

func TestPartsTextSerializesStructuredOutput(t *testing.T) {
	structured := kagenta2a.NewStructuredOutputPart(map[string]any{"answer": float64(4)}, "digest")
	toolResult := a2atype.NewDataPart(map[string]any{"name": "tool", "response": "ignored"})

	text, err := PartsText(a2atype.ContentParts{toolResult, structured})
	require.NoError(t, err)
	require.JSONEq(t, `{"answer":4}`, text)
}

// A resumed task continues from where it paused, so its earlier output is not new.
func TestResumeAssemblerContinuesThePausedTask(t *testing.T) {
	paused := &a2atype.Task{
		ID: "task-1", ContextID: "session-1",
		Status:    a2atype.TaskStatus{State: a2atype.TaskStateInputRequired},
		Artifacts: []*a2atype.Artifact{{ID: "before", Parts: a2atype.ContentParts{a2atype.NewTextPart("before")}}},
	}
	assembler := ResumeAssembler(paused)

	require.NoError(t, assembler.Apply(&a2atype.TaskArtifactUpdateEvent{
		TaskID: "task-1", ContextID: "session-1",
		Artifact: &a2atype.Artifact{ID: "after", Parts: a2atype.ContentParts{a2atype.NewTextPart("after")}},
	}))

	result, ok := assembler.Result().(*a2atype.Task)
	require.True(t, ok)
	require.Len(t, result.Artifacts, 2)
	assert.Equal(t, "before", result.Artifacts[0].Parts[0].Text())
	assert.Len(t, paused.Artifacts, 1, "the paused task is not modified")
	assert.Error(t, ResumeAssembler(paused).Apply(&a2atype.TaskStatusUpdateEvent{
		TaskID: "task-2", ContextID: "session-1", Status: a2atype.TaskStatus{State: a2atype.TaskStateWorking},
	}), "another task's events do not apply")
}
