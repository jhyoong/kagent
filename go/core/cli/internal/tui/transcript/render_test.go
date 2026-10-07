package transcript

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	kagenta2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/stretchr/testify/assert"
)

// forceColor makes styles emit ANSI, which a test binary without a terminal otherwise strips.
func forceColor(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(0) // termenv.TrueColor; termenv is not a direct dependency
	t.Cleanup(func() { lipgloss.SetColorProfile(previous) })
}

func longLog(lines int) string {
	var b strings.Builder
	for i := range lines {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func TestRenderCollapsedTool(t *testing.T) {
	tests := []struct {
		name  string
		entry ToolActivity
		width int
		want  []string
	}{
		{
			name:  "returned shows name, args and size",
			entry: ToolActivity{ID: "c1", Name: "k8s_get_logs", Args: map[string]any{"pod": "checkout-7d9f", "tail": 200}, Outcome: Returned{Response: strings.Repeat("x", 2150)}},
			width: 80,
			want:  []string{"▸ ✓ k8s_get_logs", `pod="checkout-7d9f", tail=200`, "2.1 KB"},
		},
		{
			name:  "not run says so",
			entry: ToolActivity{Name: "k8s_delete_pod", Args: map[string]any{"pod": "p"}, Outcome: NotRun{}},
			width: 80,
			want:  []string{"▸ ⊘ k8s_delete_pod", "not run"},
		},
		{
			name:  "failed is marked",
			entry: ToolActivity{Name: "get", Outcome: Returned{Response: map[string]any{"error": "boom"}, Failed: true}},
			width: 80,
			want:  []string{"▸ ✗ get"},
		},
		{
			name:  "running is marked",
			entry: ToolActivity{Name: "get", Outcome: Running{}},
			width: 80,
			want:  []string{"▸ … get", "running"},
		},
		{
			name:  "awaiting approval is marked",
			entry: ToolActivity{Name: "delete", Outcome: AwaitingApproval{}},
			width: 80,
			want:  []string{"▸ ⏸ delete", "awaiting approval"},
		},
		{
			name:  "a narrow pane truncates the preview, keeping one line",
			entry: ToolActivity{Name: "k8s_get_logs", Args: map[string]any{"pod": strings.Repeat("p", 200)}, Outcome: Returned{Response: "ok"}},
			width: 40,
			want:  []string{"▸ ✓ k8s_get_logs", "…"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Render(tt.entry, tt.width, false, false)
			plain := ansi.Strip(got)
			assert.NotContains(t, plain, "\n", "collapsed is one line")
			assert.LessOrEqual(t, ansi.StringWidth(got), tt.width)
			assert.False(t, strings.HasPrefix(plain, " "), "no left gutter")
			for _, want := range tt.want {
				assert.Contains(t, plain, want)
			}
		})
	}
}

func TestRenderExpandedTool(t *testing.T) {
	entry := ToolActivity{
		ID: "call_8f2a", Name: "k8s_get_logs",
		Args:    map[string]any{"pod": "checkout-7d9f", "tail": 200},
		Outcome: Returned{Response: map[string]any{"logs": "panic: missing env"}},
	}
	got := ansi.Strip(Render(entry, 80, true, false))

	assert.Equal(t, strings.Join([]string{
		"▾ ✓ k8s_get_logs  call_8f2a",
		"args",
		"{",
		`  "pod": "checkout-7d9f",`,
		`  "tail": 200`,
		"}",
		"result",
		"{",
		`  "logs": "panic: missing env"`,
		"}",
	}, "\n"), got)
}

func TestRenderExpandedToolTruncatesLongOutput(t *testing.T) {
	entry := ToolActivity{Name: "logs", Outcome: Returned{Response: map[string]any{"output": longLog(maxExpandedLines + 50)}}}
	got := ansi.Strip(Render(entry, 80, true, false))

	lines := strings.Split(got, "\n")
	assert.Equal(t, "… 50 more lines (y copies the full output)", lines[len(lines)-1])
	assert.Contains(t, got, "line 0\n", "a string output prints as text, not JSON")
	assert.NotContains(t, got, fmt.Sprintf("line %d\n", maxExpandedLines+10))
}

func TestRenderWrapsWithoutGutter(t *testing.T) {
	long := strings.Repeat("word ", 40)
	entries := []Entry{
		UserMessage{Text: long},
		AgentText{Text: long},
		Banner{Kind: BannerInfo, Text: long},
		Banner{Kind: BannerError, Text: long},
		ToolActivity{Name: "get", Args: map[string]any{"q": long}, Outcome: Returned{Response: long}},
		ApprovalRecord{
			Tools:     []kagenta2a.HITLTool{{ID: "a", Name: "get_logs"}, {ID: "b", Name: "delete_pod"}},
			Decisions: []kagenta2a.ToolApproval{{ID: "a", Approved: true}, {ID: "b", RejectionReason: long}},
			AskedBy:   "billing-agent",
		},
		AnswerRecord{Questions: []kagenta2a.HITLQuestion{{Question: long}}, Answers: [][]string{{"a", "b"}}},
	}
	for _, entry := range entries {
		for _, expanded := range []bool{false, true} {
			t.Run(fmt.Sprintf("%T expanded=%v", entry, expanded), func(t *testing.T) {
				got := Render(entry, 30, expanded, false)
				_, isTool := entry.(ToolActivity)
				for i, line := range strings.Split(got, "\n") {
					assert.LessOrEqual(t, ansi.StringWidth(line), 30, "line %q", ansi.Strip(line))
					// Expanded tool JSON is indented as content; nothing else may start with a space.
					if i == 0 || !isTool || !expanded {
						assert.False(t, strings.HasPrefix(ansi.Strip(line), " "), "line %q has a gutter", ansi.Strip(line))
					}
				}
			})
		}
	}
}

func TestRenderRecords(t *testing.T) {
	tests := []struct {
		name  string
		entry Entry
		want  string
	}{
		{
			name: "approval record lists each decision",
			entry: ApprovalRecord{
				Tools:     []kagenta2a.HITLTool{{ID: "a", Name: "k8s_get_logs"}, {ID: "b", Name: "k8s_delete_pod"}},
				Decisions: []kagenta2a.ToolApproval{{ID: "a", Approved: true}, {ID: "b", RejectionReason: "not in business hours"}},
				AskedBy:   "billing-agent",
			},
			want: `✓ Approved k8s_get_logs · ✗ Rejected k8s_delete_pod: "not in business hours"  (billing-agent)`,
		},
		{
			name:  "an approval without its request names the tool generically",
			entry: ApprovalRecord{Decisions: []kagenta2a.ToolApproval{{ID: "x", Approved: true}}},
			want:  "✓ Approved tool",
		},
		{
			name: "answer record pairs each question with its answer",
			entry: AnswerRecord{
				Questions: []kagenta2a.HITLQuestion{{Question: "What size?"}, {Question: "Colors?"}},
				Answers:   [][]string{{"Large"}, {"red", "blue"}},
				AskedBy:   "shop",
			},
			want: "? What size? → Large  (shop)\n? Colors? → red, blue",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ansi.Strip(Render(tt.entry, 200, false, false)))
		})
	}
}

func TestRenderSelectedMarksEntry(t *testing.T) {
	forceColor(t)
	entry := ToolActivity{Name: "get", Outcome: Running{}}
	assert.NotEqual(t, Render(entry, 80, false, false), Render(entry, 80, false, true))
	assert.Equal(t, ansi.Strip(Render(entry, 80, false, false)), ansi.Strip(Render(entry, 80, false, true)))
}

func TestPlainText(t *testing.T) {
	forceColor(t)
	long := strings.Repeat("word ", 40)
	tests := []struct {
		name  string
		entry Entry
		want  string
	}{
		{name: "user", entry: UserMessage{Text: long}, want: "You: " + long},
		{name: "agent", entry: AgentText{Text: long}, want: "Agent:\n" + long},
		{name: "banner", entry: Banner{Kind: BannerError, Text: "boom"}, want: "boom"},
		{
			name: "tool output is complete",
			entry: ToolActivity{ID: "c1", Name: "logs", Args: map[string]any{"pod": "p"},
				Outcome: Returned{Response: map[string]any{"output": longLog(maxExpandedLines + 5)}}},
			want: "✓ logs  c1\nargs\n{\n  \"pod\": \"p\"\n}\nresult\n" + strings.TrimSuffix(longLog(maxExpandedLines+5), "\n"),
		},
		{name: "not run", entry: ToolActivity{Name: "drop", Outcome: NotRun{}}, want: "⊘ drop  not run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PlainText(tt.entry)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, ansi.Strip(got), got, "no ANSI")
		})
	}
}

func TestArgPreview(t *testing.T) {
	tests := []struct {
		name  string
		args  any
		width int
		want  string
	}{
		{name: "nil is empty", args: nil, width: 40, want: ""},
		{name: "sorted keys, JSON values", args: map[string]any{"tail": 200, "pod": "p", "all": true}, width: 80, want: `all=true, pod="p", tail=200`},
		{name: "nested values are compact JSON", args: map[string]any{"sel": map[string]any{"app": "x"}}, width: 80, want: `sel={"app":"x"}`},
		{name: "truncated to width", args: map[string]any{"pod": "checkout-7d9f"}, width: 10, want: `pod="chec…`},
		{name: "no room", args: map[string]any{"pod": "p"}, width: 0, want: ""},
		{name: "a non-object is compact JSON", args: []any{1, 2}, width: 80, want: "[1,2]"},
		{name: "an empty object is empty", args: map[string]any{}, width: 80, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ArgPreview(tt.args, tt.width)
			assert.Equal(t, tt.want, got)
			assert.LessOrEqual(t, ansi.StringWidth(got), max(tt.width, 0))
		})
	}
}

func TestFolds(t *testing.T) {
	var folds Folds
	assert.False(t, folds.Expanded(0), "collapsed by default")

	folds.Toggle(1)
	assert.True(t, folds.Expanded(1))
	assert.False(t, folds.Expanded(0))

	folds.ToggleAll()
	assert.True(t, folds.Expanded(0), "toggling all expands everything")
	assert.True(t, folds.Expanded(1), "toggling all resets individual flips")

	folds.Toggle(0)
	assert.False(t, folds.Expanded(0), "an entry flips against the global state")
	assert.True(t, folds.Expanded(2))

	folds.Toggle(0)
	assert.True(t, folds.Expanded(0), "flipping twice restores")
}

func TestFoldsRemap(t *testing.T) {
	var folds Folds
	folds.Toggle(1)
	folds.Toggle(3)

	// Entry 1 is gone; entries 2 and 3 move up one.
	folds.Remap(func(old int) (int, bool) {
		if old == 1 {
			return 0, false
		}
		if old > 1 {
			return old - 1, true
		}
		return old, true
	})

	assert.False(t, folds.Expanded(0))
	assert.False(t, folds.Expanded(1))
	assert.True(t, folds.Expanded(2), "the flip follows its entry")
	assert.False(t, folds.Expanded(3))

	folds.ToggleAll()
	folds.Remap(func(old int) (int, bool) { return old + 1, true })
	assert.True(t, folds.Expanded(0), "the default is not an index")
}
