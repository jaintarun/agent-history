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

	"github.com/jaintarun/agent-history/internal/source"
	"github.com/jaintarun/agent-history/internal/source/claude"
	"github.com/jaintarun/agent-history/internal/source/codex"
	"github.com/jaintarun/agent-history/internal/source/grok"
	"github.com/jaintarun/agent-history/internal/store"
)

func TestCopyCommandsAreSafelyRendered(t *testing.T) {
	database := openLauncherStore(t)
	cwd := filepath.Join(t.TempDir(), "project's files")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		agent       string
		id          string
		permissions string
		want        string
	}{
		{
			name: "codex normal", agent: "codex",
			id:          "11111111-1111-4111-8111-111111111111",
			permissions: PermissionNormal,
			want:        "codex resume 11111111-1111-4111-8111-111111111111",
		},
		{
			name: "codex bypass", agent: "codex",
			id:          "11111111-1111-4111-8111-111111111111",
			permissions: PermissionBypass,
			want:        "codex resume --dangerously-bypass-approvals-and-sandbox 11111111-1111-4111-8111-111111111111",
		},
		{
			name: "claude normal", agent: "claude", id: "session:release.one",
			permissions: PermissionNormal,
			want:        "claude --resume session:release.one",
		},
		{
			name: "claude bypass", agent: "claude", id: "session:release.one",
			permissions: PermissionBypass,
			want:        "claude --dangerously-skip-permissions --resume session:release.one",
		},
		{
			name: "grok normal", agent: "grok",
			id:          "01a00000-0000-7000-8000-000000000011",
			permissions: PermissionNormal,
			want:        "grok --resume 01a00000-0000-7000-8000-000000000011",
		},
		{
			name: "grok always approve", agent: "grok",
			id:          "01a00000-0000-7000-8000-000000000011",
			permissions: PermissionBypass,
			want:        "grok --always-approve --resume 01a00000-0000-7000-8000-000000000011",
		},
	}
	launcher := newWithRunner(database, &fakeRunner{lookPathErr: errors.New("missing")}, codex.New(t.TempDir()), claude.New(t.TempDir()), grok.New(t.TempDir()))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sessionID := importLaunchSession(t, database, test.agent, test.id, cwd, "Title with 'quote'")
			result, err := launcher.Launch(context.Background(), sessionID, "copy", test.permissions)
			if err != nil {
				t.Fatal(err)
			}
			if result.Mode != "copy" || result.Command != test.want || result.Workspace != "" {
				t.Fatalf("copy result = %#v", result)
			}
		})
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
	result, err := launcher.Launch(context.Background(), sessionID, "auto", PermissionNormal)
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
	tests := []struct {
		name        string
		agent       string
		permissions string
		command     string
		adapter     source.Source
	}{
		{
			name: "claude normal", agent: "claude", permissions: PermissionNormal,
			command: "claude --resume safe-session", adapter: claude.New(t.TempDir()),
		},
		{
			name: "codex bypass", agent: "codex", permissions: PermissionBypass,
			command: "codex resume --dangerously-bypass-approvals-and-sandbox safe-session", adapter: codex.New(t.TempDir()),
		},
		{
			name: "grok bypass", agent: "grok", permissions: PermissionBypass,
			command: "grok --always-approve --resume safe-session", adapter: grok.New(t.TempDir()),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sessionID := importLaunchSession(t, database, test.agent, "safe-session", cwd, "Fix user's retry")
			runner := &fakeRunner{path: "/fake/cmux", output: "workspace:7\n"}
			launcher := newWithRunner(database, runner, test.adapter)
			result, err := launcher.Launch(context.Background(), sessionID, "cmux", test.permissions)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{
				"workspace", "create", "--name", "Fix user's retry", "--cwd", cwd,
				"--command", test.command, "--focus", "true",
			}
			if runner.executable != "/fake/cmux" || !reflect.DeepEqual(runner.args, want) {
				t.Fatalf("cmux invocation = %q %q, want %q", runner.executable, runner.args, want)
			}
			if result.Mode != "cmux" || result.Workspace != "workspace:7" || result.Command != "" {
				t.Fatalf("cmux result = %#v", result)
			}
		})
	}
}

func TestCmuxFailureIsReturned(t *testing.T) {
	database := openLauncherStore(t)
	sessionID := importLaunchSession(t, database, "codex", "safe-session", t.TempDir(), "Failure")
	runner := &fakeRunner{path: "/fake/cmux", runErr: errors.New("socket unavailable")}
	launcher := newWithRunner(database, runner, codex.New(t.TempDir()))
	_, err := launcher.Launch(context.Background(), sessionID, "cmux", PermissionNormal)
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
	result, err := launcher.Launch(context.Background(), sessionID, "cmux", PermissionNormal)
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
			if _, err := launcher.Launch(context.Background(), sessionID, "copy", PermissionNormal); err == nil {
				t.Fatal("Launch succeeded with untrusted metadata")
			}
		})
	}
	if _, err := launcher.Launch(context.Background(), "missing", "copy", PermissionNormal); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing session error = %v", err)
	}
}

func TestLauncherValidatesNormalSpecBeforeBypass(t *testing.T) {
	database := openLauncherStore(t)
	cwd := t.TempDir()
	nativeID := "safe-session"
	sessionID := importLaunchSession(t, database, "codex", nativeID, cwd, "Untrusted adapter")
	base := source.ResumeSpec{
		Agent: "codex", SessionID: nativeID, CWD: cwd,
		Executable: "codex", Args: []string{"resume", nativeID},
	}
	tests := []struct {
		name   string
		mutate func(*source.ResumeSpec)
	}{
		{name: "agent", mutate: func(spec *source.ResumeSpec) { spec.Agent = "claude" }},
		{name: "session ID", mutate: func(spec *source.ResumeSpec) { spec.SessionID = "other-session" }},
		{name: "working directory", mutate: func(spec *source.ResumeSpec) { spec.CWD = "/different" }},
		{name: "executable", mutate: func(spec *source.ResumeSpec) { spec.Executable = "sh" }},
		{name: "arguments", mutate: func(spec *source.ResumeSpec) {
			spec.Args = []string{"resume", "--dangerously-bypass-approvals-and-sandbox", nativeID}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := base
			spec.Args = append([]string(nil), base.Args...)
			test.mutate(&spec)
			originalArgs := append([]string(nil), spec.Args...)
			runner := &fakeRunner{path: "/fake/cmux"}
			launcher := newWithRunner(database, runner, untrustedResumeSource{spec: spec})

			_, err := launcher.Launch(context.Background(), sessionID, "cmux", PermissionBypass)
			if err == nil || !strings.Contains(err.Error(), "untrusted resume specification") {
				t.Fatalf("Launch error = %v", err)
			}
			if runner.executable != "" || len(runner.args) != 0 {
				t.Fatalf("runner invoked with %q %q", runner.executable, runner.args)
			}
			if !reflect.DeepEqual(spec.Args, originalArgs) {
				t.Fatalf("source args mutated: got %q, want %q", spec.Args, originalArgs)
			}
		})
	}
}

func TestLauncherRejectsUnknownPermissionMode(t *testing.T) {
	database := openLauncherStore(t)
	sessionID := importLaunchSession(t, database, "codex", "safe-session", t.TempDir(), "Invalid mode")
	launcher := newWithRunner(database, &fakeRunner{}, codex.New(t.TempDir()))
	if _, err := launcher.Launch(context.Background(), sessionID, "copy", "custom-flag"); err == nil {
		t.Fatal("Launch accepted an unknown permission mode")
	}
}

func TestPermissionArgsDoesNotMutateNormalSpec(t *testing.T) {
	normal := []string{"resume", "safe-session"}
	got := permissionArgs("codex", normal, PermissionBypass)
	if !reflect.DeepEqual(normal, []string{"resume", "safe-session"}) {
		t.Fatalf("normal args mutated: %q", normal)
	}
	want := []string{"resume", "--dangerously-bypass-approvals-and-sandbox", "safe-session"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bypass args = %q, want %q", got, want)
	}
}

type untrustedResumeSource struct {
	spec source.ResumeSpec
}

func (untrustedResumeSource) Name() string {
	return "codex"
}

func (untrustedResumeSource) Discover(context.Context) ([]source.Candidate, error) {
	return nil, nil
}

func (untrustedResumeSource) Read(context.Context, source.Candidate) (source.ImportedSession, error) {
	return source.ImportedSession{}, nil
}

func (s untrustedResumeSource) ResumeSpec(store.Session) (source.ResumeSpec, error) {
	return s.spec, nil
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
