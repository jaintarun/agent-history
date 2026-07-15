package cmux

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tarunjain/agent-history/internal/store"
)

func TestReconcilerMatchesOnlyExactOpenSessions(t *testing.T) {
	database := openReconcilerStore(t)
	upsertAnalyzedSession(t, database, "codex-session", "codex", "codex-native", "Codex generated title", "current")
	upsertAnalyzedSession(t, database, "claude-session", "claude", "claude-native", "Claude generated title", "current")
	home := t.TempDir()
	writeHookFixture(t, home, "codex", hookJSON("codex-native", "w1", "s1", "running"))
	writeHookFixture(t, home, "claude", hookJSON("claude-native", "w2", "missing-surface", "idle"))
	api := &fakeAPI{
		capabilities: Capabilities{AccessMode: "allowAll"},
		workspaces:   []Workspace{{ID: "w1", Title: "Codex cmux", HasCustomTitle: true}, {ID: "w2", Title: "Claude cmux"}},
		surfaces:     map[string][]Surface{"w1": {{ID: "s1", Title: "Codex tab"}}, "w2": {{ID: "s2", Title: "Claude tab"}}},
	}
	reconciler := NewReconciler(api, database, home, slog.Default())
	if err := reconciler.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	state, ok, err := database.CmuxState(context.Background(), "codex-session")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !state.Open || state.Lifecycle != "running" || state.WorkspaceTitle != "Codex cmux" || state.SurfaceTitle != "Codex tab" {
		t.Fatalf("codex cmux state = %#v, %v", state, ok)
	}
	if state, ok, err := database.CmuxState(context.Background(), "claude-session"); err != nil || ok && state.Open {
		t.Fatalf("stale Claude state = %#v, %v, %v", state, ok, err)
	}
}

func TestReconcilerRequiresAllowAll(t *testing.T) {
	database := openReconcilerStore(t)
	api := &fakeAPI{capabilities: Capabilities{AccessMode: "cmuxOnly"}}
	reconciler := NewReconciler(api, database, t.TempDir(), slog.Default())
	if err := reconciler.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "allowAll") {
		t.Fatalf("Refresh error = %v", err)
	}
	status, err := database.CmuxStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Available || status.AccessMode != "cmuxOnly" || !strings.Contains(status.Error, "allowAll") {
		t.Fatalf("status = %#v", status)
	}
}

func TestAutomaticSyncPolicy(t *testing.T) {
	tests := []struct {
		name              string
		auto              bool
		analysisStatus    string
		workspaceTitle    string
		workspaceCustom   bool
		surfaceTitle      string
		lastWorkspace     string
		lastSurface       string
		wantWorkspaceCall bool
		wantSurfaceCall   bool
	}{
		{name: "setting off", auto: false, workspaceTitle: "generic", surfaceTitle: "tab"},
		{name: "unowned workspace", auto: true, workspaceTitle: "generic", surfaceTitle: "tab", wantWorkspaceCall: true},
		{name: "previously pushed workspace", auto: true, workspaceTitle: "old", workspaceCustom: true, surfaceTitle: "tab", lastWorkspace: "old", wantWorkspaceCall: true},
		{name: "independently changed workspace", auto: true, workspaceTitle: "manual", workspaceCustom: true, surfaceTitle: "tab", lastWorkspace: "old"},
		{name: "previously pushed tab", auto: true, workspaceTitle: "Generated title", workspaceCustom: true, surfaceTitle: "old tab", lastSurface: "old tab", wantSurfaceCall: true},
		{name: "tab without provenance", auto: true, workspaceTitle: "Generated title", workspaceCustom: true, surfaceTitle: "manual tab"},
		{name: "partial analysis", auto: true, analysisStatus: "partial", workspaceTitle: "generic", surfaceTitle: "tab"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := openReconcilerStore(t)
			status := test.analysisStatus
			if status == "" {
				status = "current"
			}
			upsertAnalyzedSession(t, database, "session", "codex", "native", "Generated title", status)
			if err := database.SetSetting(context.Background(), "cmux.title_sync", fmt.Sprint(test.auto)); err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			writeHookFixture(t, home, "codex", hookJSON("native", "w1", "s1", "idle"))
			api := &fakeAPI{
				capabilities: Capabilities{AccessMode: "allowAll"},
				workspaces:   []Workspace{{ID: "w1", Title: test.workspaceTitle, HasCustomTitle: test.workspaceCustom}},
				surfaces:     map[string][]Surface{"w1": {{ID: "s1", Title: test.surfaceTitle}}},
			}
			reconciler := NewReconciler(api, database, home, slog.Default())
			if test.lastWorkspace != "" || test.lastSurface != "" {
				if err := reconciler.RefreshWithoutSync(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := database.RecordCmuxPush(context.Background(), "session", test.lastWorkspace, test.lastSurface, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			api.clearCalls()
			if err := reconciler.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			workspaceCalls, surfaceCalls := api.callCounts()
			if got := workspaceCalls > 0; got != test.wantWorkspaceCall {
				t.Fatalf("workspace renamed = %v, want %v; calls=%v", got, test.wantWorkspaceCall, api.calls)
			}
			if got := surfaceCalls > 0; got != test.wantSurfaceCall {
				t.Fatalf("surface renamed = %v, want %v; calls=%v", got, test.wantSurfaceCall, api.calls)
			}
		})
	}
}

func TestAutomaticSyncSkipsWorkspaceWithMultipleMappedSessions(t *testing.T) {
	database := openReconcilerStore(t)
	upsertAnalyzedSession(t, database, "one", "codex", "n1", "First generated", "current")
	upsertAnalyzedSession(t, database, "two", "claude", "n2", "Second generated", "current")
	if err := database.SetSetting(context.Background(), "cmux.title_sync", "true"); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	writeHookFixture(t, home, "codex", hookJSON("n1", "w1", "s1", "idle"))
	writeHookFixture(t, home, "claude", hookJSON("n2", "w1", "s2", "idle"))
	api := &fakeAPI{
		capabilities: Capabilities{AccessMode: "allowAll"},
		workspaces:   []Workspace{{ID: "w1", Title: "Shared"}},
		surfaces:     map[string][]Surface{"w1": {{ID: "s1", Title: "One"}, {ID: "s2", Title: "Two"}}},
	}
	if err := NewReconciler(api, database, home, slog.Default()).Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != 0 {
		t.Fatalf("automatic calls = %v, want none", api.calls)
	}
}

func TestManualPushRenamesExactTabButNotSharedWorkspace(t *testing.T) {
	database := openReconcilerStore(t)
	upsertAnalyzedSession(t, database, "one", "codex", "n1", "First generated", "current")
	upsertAnalyzedSession(t, database, "two", "claude", "n2", "Second generated", "current")
	home := t.TempDir()
	writeHookFixture(t, home, "codex", hookJSON("n1", "w1", "s1", "idle"))
	writeHookFixture(t, home, "claude", hookJSON("n2", "w1", "s2", "idle"))
	api := &fakeAPI{
		capabilities: Capabilities{AccessMode: "allowAll"},
		workspaces:   []Workspace{{ID: "w1", Title: "Shared", HasCustomTitle: true}},
		surfaces:     map[string][]Surface{"w1": {{ID: "s1", Title: "One"}, {ID: "s2", Title: "Two"}}},
	}
	reconciler := NewReconciler(api, database, home, slog.Default())
	if err := reconciler.PushTitle(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	workspaceCalls, surfaceCalls := api.callCounts()
	if workspaceCalls != 0 || surfaceCalls != 1 || api.surfaces["w1"][0].Title != "First generated" {
		t.Fatalf("calls = %v surfaces=%#v", api.calls, api.surfaces)
	}
	state, ok, err := database.CmuxState(context.Background(), "one")
	if err != nil || !ok || state.LastPushedSurfaceTitle != "First generated" || state.LastPushedWorkspaceTitle != "" {
		t.Fatalf("state = %#v, %v, %v", state, ok, err)
	}
}

type fakeAPI struct {
	mu           sync.Mutex
	capabilities Capabilities
	err          error
	workspaces   []Workspace
	surfaces     map[string][]Surface
	calls        []string
}

func (f *fakeAPI) Capabilities(context.Context) (Capabilities, error) {
	return f.capabilities, f.err
}

func (f *fakeAPI) Workspaces(context.Context) ([]Workspace, error) {
	return append([]Workspace(nil), f.workspaces...), f.err
}

func (f *fakeAPI) Surfaces(_ context.Context, workspaceID string) ([]Surface, error) {
	return append([]Surface(nil), f.surfaces[workspaceID]...), f.err
}

func (f *fakeAPI) RenameWorkspace(_ context.Context, workspaceID, title string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "workspace:"+workspaceID+":"+title)
	for index := range f.workspaces {
		if f.workspaces[index].ID == workspaceID {
			f.workspaces[index].Title = title
			f.workspaces[index].CustomTitle = title
			f.workspaces[index].HasCustomTitle = true
		}
	}
	return f.err
}

func (f *fakeAPI) RenameSurface(_ context.Context, workspaceID, surfaceID, title string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "surface:"+surfaceID+":"+title)
	for index := range f.surfaces[workspaceID] {
		if f.surfaces[workspaceID][index].ID == surfaceID {
			f.surfaces[workspaceID][index].Title = title
		}
	}
	return f.err
}

func (f *fakeAPI) clearCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *fakeAPI) callCounts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var workspaces, surfaces int
	for _, call := range f.calls {
		if strings.HasPrefix(call, "workspace:") {
			workspaces++
		}
		if strings.HasPrefix(call, "surface:") {
			surfaces++
		}
	}
	return workspaces, surfaces
}

func openReconcilerStore(t *testing.T) *store.Store {
	t.Helper()
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func upsertAnalyzedSession(t *testing.T, database *store.Store, id, agent, nativeID, title, status string) {
	t.Helper()
	started := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	session := store.Session{
		ID: id, Agent: agent, NativeSessionID: nativeID, SourcePath: "/tmp/" + id,
		SourceSize: 1, SourceMTime: started, SourceHash: "hash-" + id,
		WorkingDirectory: "/tmp/project", StartedAt: started, LastActiveAt: started.Add(time.Hour),
	}
	if err := database.UpsertSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if err := database.ReplaceAnalysis(context.Background(), id, store.Analysis{
		Title: title, Summary: "summary", Status: "current", Provider: "fake", Model: "test",
		PromptVersion: "v1", AnalyzedAt: started.Add(time.Hour), AnalyzedHash: "analysis-" + id,
	}); err != nil {
		t.Fatal(err)
	}
	if status != "current" {
		if err := database.SetAnalysisStatus(context.Background(), id, status, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func hookJSON(nativeID, workspaceID, surfaceID, lifecycle string) string {
	return fmt.Sprintf("{\"sessions\":{\"one\":{\"sessionId\":%q,\"workspaceId\":%q,\"surfaceId\":%q,\"agentLifecycle\":%q,\"updatedAt\":100}}}",
		nativeID, workspaceID, surfaceID, lifecycle)
}
