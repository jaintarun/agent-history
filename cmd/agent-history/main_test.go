package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tarunjain/agent-history/internal/source"
	"github.com/tarunjain/agent-history/internal/store"
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
	for _, flag := range []string{"-bind", "-database", "-config", "-open-browser", "-no-open", "-scan-on-start", "-scan-interval"} {
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

func TestServePrintsURLAndStopsCleanly(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "history.db")
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
	if !strings.HasPrefix(output, "http://127.0.0.1:") || !strings.Contains(output, "/") {
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
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestScanLoopRunsStartupAndPeriodicScansUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	scanner := &countingScanner{calls: make(chan struct{}, 3)}
	done := startScanLoop(ctx, scanner, true, 10*time.Millisecond, nil)
	for range 2 {
		select {
		case <-scanner.calls:
		case <-time.After(time.Second):
			t.Fatal("scheduled scan did not run")
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
