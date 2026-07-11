package analyze

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexCLIGenerateUsesEphemeralReadOnlySchemaInvocation(t *testing.T) {
	executable, logPath, stdinPath := fakeCodexExecutable(t)
	t.Setenv("FAKE_CODEX_OUTPUT", `{"title":"ok"}`)
	analyzer := NewCodexCLI(executable)
	request := StructuredRequest{Kind: RequestLeaf, Prompt: "untrusted transcript", Schema: json.RawMessage(`{"type":"object"}`)}

	result, err := analyzer.Generate(context.Background(), "cheap-model", request)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `{"title":"ok"}` {
		t.Fatalf("result = %s", result)
	}
	args, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	argumentLog := string(args)
	for _, required := range []string{
		"exec", "--ephemeral", "--ignore-user-config", "--ignore-rules",
		"--skip-git-repo-check", "--sandbox", "read-only", "--output-schema",
		"--output-last-message", "--model", "cheap-model", "-",
	} {
		if !strings.Contains(argumentLog, required+"\n") {
			t.Errorf("argument log missing %q:\n%s", required, argumentLog)
		}
	}
	stdin, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stdin) != request.Prompt {
		t.Fatalf("stdin = %q", stdin)
	}
}

func TestCodexCLIRejectsInvalidJSON(t *testing.T) {
	executable, _, _ := fakeCodexExecutable(t)
	t.Setenv("FAKE_CODEX_OUTPUT", `not json`)
	_, err := NewCodexCLI(executable).Generate(context.Background(), "model", testStructuredRequest())
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("error = %v, want invalid JSON", err)
	}
}

func TestCodexCLINonzeroExitBoundsStderr(t *testing.T) {
	executable, _, _ := fakeCodexExecutable(t)
	t.Setenv("FAKE_CODEX_STDERR", strings.Repeat("failure", 200))
	t.Setenv("FAKE_CODEX_EXIT", "7")
	analyzer := NewCodexCLI(executable)
	analyzer.MaxOutputBytes = 128
	_, err := analyzer.Generate(context.Background(), "model", testStructuredRequest())
	if err == nil || !strings.Contains(err.Error(), "exit status 7") || !strings.Contains(err.Error(), "[truncated]") {
		t.Fatalf("error = %v, want bounded nonzero exit", err)
	}
	if len(err.Error()) > 400 {
		t.Fatalf("bounded error length = %d", len(err.Error()))
	}
}

func TestCodexCLITimeoutAndCancellation(t *testing.T) {
	executable, _, _ := fakeCodexExecutable(t)
	t.Setenv("FAKE_CODEX_SLEEP", "1")
	analyzer := NewCodexCLI(executable)
	analyzer.Timeout = 30 * time.Millisecond
	if _, err := analyzer.Generate(context.Background(), "model", testStructuredRequest()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := analyzer.Generate(ctx, "model", testStructuredRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func testStructuredRequest() StructuredRequest {
	return StructuredRequest{Kind: RequestLeaf, Prompt: "data", Schema: json.RawMessage(`{"type":"object"}`)}
}

func fakeCodexExecutable(t *testing.T) (string, string, string) {
	t.Helper()
	directory := t.TempDir()
	executable := filepath.Join(directory, "fake-codex")
	logPath := filepath.Join(directory, "args.log")
	stdinPath := filepath.Join(directory, "stdin.log")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$@" > "$FAKE_CODEX_LOG"
cat > "$FAKE_CODEX_STDIN"
output=''
previous=''
for argument in "$@"; do
  if [ "$previous" = '--output-last-message' ]; then output="$argument"; fi
  previous="$argument"
done
if [ -n "${FAKE_CODEX_SLEEP:-}" ]; then sleep "$FAKE_CODEX_SLEEP"; fi
if [ -n "${FAKE_CODEX_STDERR:-}" ]; then printf '%s' "$FAKE_CODEX_STDERR" >&2; fi
if [ -n "$output" ]; then
  if [ "${FAKE_CODEX_OUTPUT+x}" = x ]; then
    printf '%s' "$FAKE_CODEX_OUTPUT" > "$output"
  else
    printf '%s' '{}' > "$output"
  fi
fi
exit "${FAKE_CODEX_EXIT:-0}"
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CODEX_LOG", logPath)
	t.Setenv("FAKE_CODEX_STDIN", stdinPath)
	return executable, logPath, stdinPath
}
