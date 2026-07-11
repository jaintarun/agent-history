package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	for _, flag := range []string{"-bind", "-database", "-config", "-open-browser", "-no-open", "-scan-interval"} {
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
