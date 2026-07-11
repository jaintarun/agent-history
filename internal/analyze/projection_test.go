package analyze

import (
	"strings"
	"testing"
	"time"

	"github.com/tarunjain/agent-history/internal/store"
)

func TestBuildTurnsGroupsUserAssistantAndTools(t *testing.T) {
	now := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	messages := []store.Message{
		{Sequence: 0, Timestamp: now, Role: "user", Text: "Fix the refresh bug."},
		{Sequence: 1, Timestamp: now.Add(time.Minute), Role: "assistant", Text: "I will inspect it."},
		{Sequence: 2, Timestamp: now.Add(2 * time.Minute), Role: "tool", Text: "go test ./...", ToolName: "exec"},
		{Sequence: 3, Timestamp: now.Add(3 * time.Minute), Role: "assistant", Text: "The fix passes."},
		{Sequence: 4, Timestamp: now.Add(4 * time.Minute), Role: "user", Text: "Add a regression test."},
	}

	turns := BuildTurns(messages)

	if len(turns) != 2 {
		t.Fatalf("turn count = %d, want 2", len(turns))
	}
	if turns[0].StartSequence != 0 || turns[0].EndSequence != 3 || turns[1].StartSequence != 4 {
		t.Fatalf("turn ranges = %#v", turns)
	}
}

func TestCompactProjectionCollapsesRepeatedLogsAndDiffBodies(t *testing.T) {
	turn := Turn{Messages: []store.Message{
		{Sequence: 0, Role: "user", Text: "Run the tests."},
		{Sequence: 1, Role: "tool", ToolName: "exec", Text: "same log\nsame log\nsame log\nerror: timeout\ncontext"},
		{Sequence: 2, Role: "tool", ToolName: "apply_patch", Text: "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new"},
	}}

	projection := CompactTurn(turn)

	if !strings.Contains(projection, "same log [repeated 3 times]") {
		t.Fatalf("projection did not collapse logs:\n%s", projection)
	}
	if !strings.Contains(projection, "diff files: a.go") || strings.Contains(projection, "-old") {
		t.Fatalf("projection did not compact diff:\n%s", projection)
	}
}

func TestBoundaryScoringDoesNotSplitOnIdleAlone(t *testing.T) {
	before := Turn{StartedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC), EndedAt: time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC), UserText: "Continue implementing the session index"}
	after := Turn{StartedAt: before.EndedAt.Add(24 * time.Hour), EndedAt: before.EndedAt.Add(25 * time.Hour), UserText: "Continue the session index implementation"}

	decision := ScoreBoundary(before, after)

	if decision.Strong || decision.Score >= ambiguousBoundaryScore {
		t.Fatalf("idle continuation boundary = %#v, want same topic", decision)
	}
}

func TestBoundaryScoringSplitsExplicitGoalAndRepositoryShift(t *testing.T) {
	before := Turn{UserText: "Finish authentication in /work/api", Projection: "files auth/token.go"}
	after := Turn{UserText: "New task: redesign the vault in /work/history", Projection: "files history/vault.go"}

	decision := ScoreBoundary(before, after)

	if !decision.Strong {
		t.Fatalf("explicit goal boundary = %#v, want strong", decision)
	}
}
