package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tarunjain/agent-history/internal/source"
	"github.com/tarunjain/agent-history/internal/source/claude"
	"github.com/tarunjain/agent-history/internal/source/codex"
	"github.com/tarunjain/agent-history/internal/store"
)

func TestCopyCommandsAreSafelyRendered(t *testing.T) {
	database := openLauncherStore(t)
	cwd := filepath.Join(t.TempDir(), "project's files")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		agent string
		id    string
		want  string
	}{
		{agent: "codex", id: "11111111-1111-4111-8111-111111111111", want: "codex resume 11111111-1111-4111-8111-111111111111"},
		{agent: "claude", id: "session:release.one", want: "claude --resume session:release.one"},
	}
	launcher := newWithRunner(database, &fakeRunner{lookPathErr: errors.New("missing")}, codex.New(t.TempDir()), claude.New(t.TempDir()))
	for _, test := range tests {
		sessionID := importLaunchSession(t, database, test.agent, test.id, cwd, "Title with 'quote'")
		result, err := launcher.Launch(context.Background(), sessionID, "copy")
		if err != nil {
			t.Fatal(err)
		}
		if result.Mode != "copy" || result.Command != test.want || result.Workspace != "" {
			t.Errorf("%s copy result = %#v", test.agent, result)
		}
	}
}

func TestShellQuote(t *testing.T) {
	tests := map[string]string{
		"simple/path-1": "simple/path-1",
		"two words":     "'two words'",
		"it's ready":    `'it'"'"'s ready'`,
		"":              "''",
	}
	for input, want := range tests {
		if got := shellQuote(input); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAutoFallsBackWhenCmuxIsMissing(t *testing.T) {
	database := openLauncherStore(t)
	cwd := t.TempDir()
	sessionID := importLaunchSession(t, database, "codex", "safe-session", cwd, "Fallback")
	launcher := newWithRunner(database, &fakeRunner{lookPathErr: errors.New("not found")}, codex.New(t.TempDir()))
	result, err := launcher.Launch(context.Background(), sessionID, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != "copy" || result.Command != "codex resume safe-session" {
		t.Fatalf("fallback result = %#v", result)
	}
}

func TestCmuxLaunchUsesExactTrustedArgv(t *testing.T) {
	database := openLauncherStore(t)
	cwd := filepath.Join(t.TempDir(), "folder with spaces")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionID := importLaunchSession(t, database, "claude", "safe-session", cwd, "Fix user's retry")
	runner := &fakeRunner{path: "/fake/cmux", output: "workspace:7\n"}
	launcher := newWithRunner(database, runner, claude.New(t.TempDir()))
	result, err := launcher.Launch(context.Background(), sessionID, "cmux")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"workspace", "create", "--name", "Fix user's retry", "--cwd", cwd,
		"--command", "claude --resume safe-session", "--focus", "true",
	}
	if runner.executable != "/fake/cmux" || !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("cmux invocation = %q %q, want %q", runner.executable, runner.args, want)
	}
	if result.Mode != "cmux" || result.Workspace != "workspace:7" || result.Command != "" {
		t.Fatalf("cmux result = %#v", result)
	}
}

func TestCmuxFailureIsReturned(t *testing.T) {
	database := openLauncherStore(t)
	sessionID := importLaunchSession(t, database, "codex", "safe-session", t.TempDir(), "Failure")
	runner := &fakeRunner{path: "/fake/cmux", runErr: errors.New("socket unavailable")}
	launcher := newWithRunner(database, runner, codex.New(t.TempDir()))
	_, err := launcher.Launch(context.Background(), sessionID, "cmux")
	if err == nil || !strings.Contains(err.Error(), "socket unavailable") {
		t.Fatalf("Launch error = %v", err)
	}
}

func TestOSRunnerInvokesFakeCmuxExecutable(t *testing.T) {
	database := openLauncherStore(t)
	cwd := t.TempDir()
	sessionID := importLaunchSession(t, database, "codex", "safe-session", cwd, "Fake executable")
	binDir := t.TempDir()
	argsPath := filepath.Join(t.TempDir(), "args.txt")
	executable := filepath.Join(binDir, "cmux")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CMUX_ARGS_FILE\"\nprintf 'workspace:9\\n'\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("CMUX_ARGS_FILE", argsPath)
	launcher := New(database, codex.New(t.TempDir()))
	result, err := launcher.Launch(context.Background(), sessionID, "cmux")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"workspace", "create", "--name", "Fake executable", "--cwd", cwd,
		"--command", "codex resume safe-session", "--focus", "true", "",
	}, "\n")
	if string(got) != want {
		t.Fatalf("fake cmux argv = %q, want %q", got, want)
	}
	if result.Workspace != "workspace:9" {
		t.Fatalf("result = %#v", result)
	}
}

func TestLauncherRejectsUntrustedSessionMetadata(t *testing.T) {
	database := openLauncherStore(t)
	validCWD := t.TempDir()
	tests := []struct {
		name  string
		agent string
		id    string
		cwd   string
	}{
		{name: "unsafe ID", agent: "codex", id: "id; touch /tmp/no", cwd: validCWD},
		{name: "relative cwd", agent: "codex", id: "safe-relative", cwd: "relative/path"},
		{name: "missing cwd", agent: "codex", id: "safe-missing", cwd: filepath.Join(t.TempDir(), "missing")},
		{name: "adapter missing", agent: "claude", id: "safe-claude", cwd: validCWD},
	}
	launcher := newWithRunner(database, &fakeRunner{lookPathErr: errors.New("missing")}, codex.New(t.TempDir()))
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sessionID := importLaunchSession(t, database, test.agent, test.id, test.cwd, fmt.Sprintf("Invalid %d", index))
			if _, err := launcher.Launch(context.Background(), sessionID, "copy"); err == nil {
				t.Fatal("Launch succeeded with untrusted metadata")
			}
		})
	}
	if _, err := launcher.Launch(context.Background(), "missing", "copy"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing session error = %v", err)
	}
}

type fakeRunner struct {
	path        string
	lookPathErr error
	executable  string
	args        []string
	output      string
	runErr      error
}

func (r fakeRunner) LookPath(string) (string, error) {
	return r.path, r.lookPathErr
}

func (r *fakeRunner) Run(_ context.Context, executable string, args []string) ([]byte, error) {
	r.executable = executable
	r.args = append([]string(nil), args...)
	return []byte(r.output), r.runErr
}

func openLauncherStore(t *testing.T) *store.Store {
	t.Helper()
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func importLaunchSession(t *testing.T, database *store.Store, agent, nativeID, cwd, title string) string {
	t.Helper()
	id := source.StableID(agent, nativeID+title)
	now := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	session := store.Session{
		ID: id, Agent: agent, NativeSessionID: nativeID, SourcePath: "/tmp/" + id,
		SourceSize: 1, SourceMTime: now, SourceHash: "hash-" + id,
		WorkingDirectory: cwd, Title: title, StartedAt: now, LastActiveAt: now,
	}
	if err := database.UpsertSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if title != "" {
		if err := database.ReplaceAnalysis(context.Background(), id, store.Analysis{
			Title: title, Summary: "launcher fixture", Status: "current",
			Provider: "fake", Model: "test", PromptVersion: "v1", AnalyzedHash: "analysis-" + id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return id
}
