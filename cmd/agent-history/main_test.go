package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaintarun/agent-history/internal/analyze"
	"github.com/jaintarun/agent-history/internal/source"
	"github.com/jaintarun/agent-history/internal/store"
)

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"version"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(version) code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "agent-history dev\n"; got != want {
		t.Fatalf("run(version) stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("run(version) stderr = %q, want empty", stderr.String())
	}
}

func TestServeHelpDocumentsFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"serve", "--help"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(serve --help) code = %d, want 0; stderr = %q", code, stderr.String())
	}
	help := stdout.String()
	for _, flag := range []string{"-bind", "-database", "-config", "-open-browser", "-no-open", "-no-url-token", "-scan-on-start", "-scan-interval", "-analyze-pending"} {
		if !strings.Contains(help, flag) {
			t.Errorf("run(serve --help) output missing %q:\n%s", flag, help)
		}
	}
}

func TestUnknownCommandFails(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"unknown"}, &stdout, &stderr)

	if code == 0 {
		t.Fatal("run(unknown) code = 0, want nonzero")
	}
	if stdout.Len() != 0 {
		t.Fatalf("run(unknown) stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("run(unknown) stderr = %q, want unknown command error", stderr.String())
	}
}

func TestEvalRequiresExplicitProviderUsageAcknowledgement(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runContext(context.Background(), []string{"eval"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--allow-provider-usage") {
		t.Fatalf("eval guard code/stdout/stderr = %d, %q, %q", code, stdout.String(), stderr.String())
	}
}

func TestScanCodexImportsFixture(t *testing.T) {
	home := t.TempDir()
	transcript := filepath.Join(home, "sessions", "2026", "07", "rollout.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "" +
		`{"timestamp":"2026-07-01T10:00:00Z","type":"session_meta","payload":{"id":"44444444-4444-4444-8444-444444444444","cwd":"/tmp/project"}}` + "\n" +
		`{"timestamp":"2026-07-01T10:01:00Z","type":"event_msg","payload":{"type":"user_message","message":"Find this session."}}` + "\n"
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	database := filepath.Join(t.TempDir(), "history.db")
	var stdout, stderr bytes.Buffer

	code := run([]string{"scan", "--agent", "codex", "--database", database}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(scan) code = %d, stderr = %q", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "discovered=1 imported=1") {
		t.Fatalf("run(scan) stdout = %q", got)
	}
}

func TestScanGrokImportsFixture(t *testing.T) {
	home := t.TempDir()
	sessionDir := filepath.Join(home, "sessions", "%2Ftmp%2Fproject", "01a00000-0000-7000-8000-000000000010")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	summary := `{"created_at":"2026-09-01T10:00:00Z","last_active_at":"2026-09-01T10:01:00Z","info":{"id":"01a00000-0000-7000-8000-000000000010","cwd":"/tmp/project"}}`
	updates := `{"method":"session/update","timestamp":1788256800,"params":{"sessionId":"01a00000-0000-7000-8000-000000000010","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Find this Grok session."}}}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "summary.json"), []byte(summary), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "updates.jsonl"), []byte(updates), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", home)
	databasePath := filepath.Join(t.TempDir(), "history.db")
	var stdout, stderr bytes.Buffer

	code := run([]string{"scan", "--agent", "grok", "--database", databasePath}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(scan) code = %d, stderr = %q", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "discovered=1 imported=1") {
		t.Fatalf("run(scan) stdout = %q", got)
	}
	database, err := store.Open(context.Background(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	detail, err := database.GetSession(context.Background(), source.StableID("grok", "01a00000-0000-7000-8000-000000000010"))
	if err != nil {
		t.Fatal(err)
	}
	if detail.Session.Agent != "grok" || len(detail.Messages) != 1 || detail.Messages[0].Text != "Find this Grok session." {
		t.Fatalf("imported Grok session = %#v, messages = %#v", detail.Session, detail.Messages)
	}
}

func TestServePrintsURLAndStopsCleanly(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "history.db")
	t.Setenv("CMUX_SOCKET_PATH", filepath.Join(t.TempDir(), "missing-cmux.sock"))
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("[analysis]\nprovider = \"codex-cli\"\nmodel = \"configured-model\"\nauto = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close()
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runContext(ctx, []string{
			"serve", "--no-open", "--bind", "127.0.0.1:0",
			"--no-url-token",
			"--database", databasePath, "--config", configPath,
			"--scan-on-start=false", "--scan-interval", "0",
		}, stdoutWriter, &stderr)
		_ = stdoutWriter.Close()
	}()
	line := make(chan string, 1)
	go func() {
		value, _ := bufio.NewReader(stdoutReader).ReadString('\n')
		line <- value
	}()

	output := <-line
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("serve exit code = %d stderr=%s", code, stderr.String())
	}
	parsed, err := url.Parse(strings.TrimSpace(output))
	if err != nil || parsed.Hostname() != "127.0.0.1" || parsed.Path != "/" {
		t.Fatalf("serve output = %q", output)
	}
	database, err := store.Open(context.Background(), databasePath)
	if err != nil {
		t.Fatalf("reopen database after shutdown: %v", err)
	}
	model, ok, err := database.Setting(context.Background(), "analysis.model")
	if err != nil || !ok || model != "configured-model" {
		t.Fatalf("seeded model = %q, %v, %v", model, ok, err)
	}
	cmuxSync, ok, err := database.Setting(context.Background(), "cmux.title_sync")
	if err != nil || !ok || cmuxSync != "false" {
		t.Fatalf("seeded cmux title sync = %q, %v, %v", cmuxSync, ok, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEnqueuePendingAnalysesQueuesEachCandidate(t *testing.T) {
	database := &fakePendingStore{ids: []string{"session-2", "session-1"}}
	queue := &fakeAnalysisQueue{}
	options := analyze.Options{Provider: "codex-cli", Model: "test", PromptVersion: "v1", NormalizerVersion: "v1"}

	queued, err := enqueuePendingAnalyses(context.Background(), database, queue, func(context.Context) (analyze.Options, error) {
		return options, nil
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if queued != 2 || strings.Join(queue.ids, ",") != "session-2,session-1" {
		t.Fatalf("queued=%d ids=%v", queued, queue.ids)
	}
	if queue.jobs[0].Model != "test" {
		t.Fatalf("queued options = %#v", queue.jobs)
	}
}

func TestEnqueuePendingAnalysesLoadsCurrentOptionsForEachBatch(t *testing.T) {
	database := &fakePendingStore{ids: []string{"session-1"}}
	queue := &fakeAnalysisQueue{}
	current := analyze.Options{
		Provider: "codex-cli", Model: "first", PromptVersion: "v1", NormalizerVersion: "v1",
	}
	load := func(context.Context) (analyze.Options, error) { return current, nil }

	if _, err := enqueuePendingAnalyses(context.Background(), database, queue, load, false); err != nil {
		t.Fatal(err)
	}
	current.Provider, current.Model = "claude-cli", "haiku"
	if _, err := enqueuePendingAnalyses(context.Background(), database, queue, load, false); err != nil {
		t.Fatal(err)
	}
	if len(queue.jobs) != 2 || queue.jobs[0].Provider != "codex-cli" || queue.jobs[1].Provider != "claude-cli" {
		t.Fatalf("jobs = %#v", queue.jobs)
	}
}

func TestPreferredAnalysisUsesInstalledCLI(t *testing.T) {
	for _, test := range []struct {
		name          string
		codex, claude bool
		provider      string
		model         string
	}{
		{name: "both", codex: true, claude: true, provider: "codex-cli", model: "gpt-5.4-mini"},
		{name: "codex", codex: true, provider: "codex-cli", model: "gpt-5.4-mini"},
		{name: "claude", claude: true, provider: "claude-cli", model: "haiku"},
		{name: "neither", provider: "codex-cli", model: "gpt-5.4-mini"},
	} {
		t.Run(test.name, func(t *testing.T) {
			providers := []analyze.Provider{
				{ID: "codex-cli", DefaultModel: "gpt-5.4-mini", Available: test.codex},
				{ID: "claude-cli", DefaultModel: "haiku", Available: test.claude},
			}
			got := preferredAnalysis(providers)
			if got.Provider != test.provider || got.Model != test.model {
				t.Fatalf("preferred analysis = %#v", got)
			}
		})
	}
}

func TestConfiguredAnalysisUsesProviderDefaultAndValidatesInput(t *testing.T) {
	providers := []analyze.Provider{
		{ID: "codex-cli", DefaultModel: "gpt-5.4-mini", Available: true},
		{ID: "claude-cli", DefaultModel: "haiku", Available: true},
	}
	for _, test := range []struct {
		name, provider, model   string
		wantProvider, wantModel string
		wantErr                 bool
	}{
		{name: "Claude default", provider: "claude-cli", wantProvider: "claude-cli", wantModel: "haiku"},
		{name: "explicit model", provider: "claude-cli", model: "sonnet", wantProvider: "claude-cli", wantModel: "sonnet"},
		{name: "unsupported provider", provider: "shell", model: "anything", wantErr: true},
		{name: "blank model", provider: "codex-cli", model: " ", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := configuredAnalysis(providers, test.provider, test.model)
			if (err != nil) != test.wantErr {
				t.Fatalf("configured analysis error = %v, wantErr = %v", err, test.wantErr)
			}
			if err == nil && (got.Provider != test.wantProvider || got.Model != test.wantModel) {
				t.Fatalf("configured analysis = %#v", got)
			}
		})
	}
}

func TestAnalysisSettingsLoadsPersistedProvider(t *testing.T) {
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.SetSettings(context.Background(), map[string]string{
		"analysis.provider": "claude-cli",
		"analysis.model":    "sonnet",
		"analysis.auto":     "false",
	}); err != nil {
		t.Fatal(err)
	}

	options, auto, err := analysisSettings(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	if options.Provider != "claude-cli" || options.Model != "sonnet" || auto {
		t.Fatalf("settings = %#v, auto = %v", options, auto)
	}
}

func TestScanLoopRunsStartupAndPeriodicScansUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	scanner := &countingScanner{calls: make(chan struct{}, 3)}
	afterScans := make(chan struct{}, 3)
	done := startScanLoop(ctx, scanner, true, 10*time.Millisecond, nil, func(context.Context) {
		afterScans <- struct{}{}
	})
	for range 2 {
		select {
		case <-scanner.calls:
		case <-time.After(time.Second):
			t.Fatal("scheduled scan did not run")
		}
		select {
		case <-afterScans:
		case <-time.After(time.Second):
			t.Fatal("post-scan action did not run")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scan loop did not stop after cancellation")
	}
	if scanner.maximum.Load() != 1 {
		t.Fatalf("maximum scan concurrency = %d", scanner.maximum.Load())
	}
}

type countingScanner struct {
	calls   chan struct{}
	active  atomic.Int32
	maximum atomic.Int32
}

type fakePendingStore struct {
	ids []string
}

func (s *fakePendingStore) PendingAnalysisSessionIDs(context.Context, bool) ([]string, error) {
	return s.ids, nil
}

type fakeAnalysisQueue struct {
	ids  []string
	jobs []analyze.Options
}

func (q *fakeAnalysisQueue) Enqueue(_ context.Context, id string, options analyze.Options) (<-chan error, error) {
	q.ids = append(q.ids, id)
	q.jobs = append(q.jobs, options)
	done := make(chan error)
	close(done)
	return done, nil
}

func (s *countingScanner) Scan(ctx context.Context, _ string) (source.ScanReport, error) {
	active := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		maximum := s.maximum.Load()
		if active <= maximum || s.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	select {
	case s.calls <- struct{}{}:
	case <-ctx.Done():
		return source.ScanReport{}, ctx.Err()
	}
	return source.ScanReport{}, nil
}
