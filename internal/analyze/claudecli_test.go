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

func TestClaudeCLIGenerateUsesSafeNonPersistentSchemaInvocation(t *testing.T) {
	executable, argsPath, stdinPath := fakeClaudeExecutable(t)
	t.Setenv("FAKE_CLAUDE_OUTPUT", `{"type":"result","structured_output":{"title":"ok"}}`)
	analyzer := NewClaudeCLI(executable)
	request := StructuredRequest{
		Kind: RequestLeaf, Prompt: "untrusted transcript",
		Schema: json.RawMessage(`{"type":"object"}`),
	}

	result, err := analyzer.Generate(context.Background(), "haiku", request)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `{"title":"ok"}` {
		t.Fatalf("result = %s", result)
	}
	arguments, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"--safe-mode", "-p", "--no-session-persistence", "--tools", "",
		"--disable-slash-commands", "--permission-mode", "dontAsk", "--max-turns", "1",
		"--output-format", "json", "--json-schema", string(request.Schema), "--model", "haiku",
	} {
		if !strings.Contains(string(arguments), required+"\n") {
			t.Errorf("argument log missing %q:\n%s", required, arguments)
		}
	}
	if strings.Contains(string(arguments), "--bare\n") {
		t.Fatalf("--bare present:\n%s", arguments)
	}
	stdin, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stdin) != request.Prompt {
		t.Fatalf("stdin = %q", stdin)
	}
}

func TestClaudeCLIRejectsInvalidStructuredOutput(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		want   string
	}{
		{name: "invalid envelope", output: `{broken`, want: "invalid JSON"},
		{name: "missing output", output: `{"type":"result"}`, want: "structured_output is missing"},
		{name: "null output", output: `{"structured_output":null}`, want: "structured_output is missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			executable, _, _ := fakeClaudeExecutable(t)
			t.Setenv("FAKE_CLAUDE_OUTPUT", test.output)
			_, err := NewClaudeCLI(executable).Generate(context.Background(), "haiku", testStructuredRequest())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestClaudeCLIRejectsOversizedOutput(t *testing.T) {
	executable, _, _ := fakeClaudeExecutable(t)
	t.Setenv("FAKE_CLAUDE_OUTPUT", strings.Repeat("x", 200))
	analyzer := NewClaudeCLI(executable)
	analyzer.MaxOutputBytes = 128
	_, err := analyzer.Generate(context.Background(), "haiku", testStructuredRequest())
	if err == nil || !strings.Contains(err.Error(), "exceeds 128 bytes") {
		t.Fatalf("error = %v, want bounded output error", err)
	}
}

func TestClaudeCLINonzeroExitBoundsStderr(t *testing.T) {
	executable, _, _ := fakeClaudeExecutable(t)
	t.Setenv("FAKE_CLAUDE_STDERR", strings.Repeat("failure", 200))
	t.Setenv("FAKE_CLAUDE_EXIT", "7")
	analyzer := NewClaudeCLI(executable)
	analyzer.MaxOutputBytes = 128
	_, err := analyzer.Generate(context.Background(), "haiku", testStructuredRequest())
	if err == nil || !strings.Contains(err.Error(), "exit status 7") || !strings.Contains(err.Error(), "[truncated]") {
		t.Fatalf("error = %v, want bounded nonzero exit", err)
	}
	if len(err.Error()) > 400 {
		t.Fatalf("bounded error length = %d", len(err.Error()))
	}
}

func TestClaudeCLITimeoutAndCancellation(t *testing.T) {
	executable, _, _ := fakeClaudeExecutable(t)
	t.Setenv("FAKE_CLAUDE_SLEEP", "1")
	analyzer := NewClaudeCLI(executable)
	analyzer.Timeout = 30 * time.Millisecond
	if _, err := analyzer.Generate(context.Background(), "haiku", testStructuredRequest()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := analyzer.Generate(ctx, "haiku", testStructuredRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func fakeClaudeExecutable(t *testing.T) (string, string, string) {
	t.Helper()
	directory := t.TempDir()
	executable := filepath.Join(directory, "fake-claude")
	argsPath := filepath.Join(directory, "args.log")
	stdinPath := filepath.Join(directory, "stdin.log")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$@" > "$FAKE_CLAUDE_ARGS"
cat > "$FAKE_CLAUDE_STDIN"
if [ -n "${FAKE_CLAUDE_SLEEP:-}" ]; then sleep "$FAKE_CLAUDE_SLEEP"; fi
if [ -n "${FAKE_CLAUDE_STDERR:-}" ]; then printf '%s' "$FAKE_CLAUDE_STDERR" >&2; fi
if [ "${FAKE_CLAUDE_OUTPUT+x}" = x ]; then printf '%s' "$FAKE_CLAUDE_OUTPUT"; else printf '%s' '{"structured_output":{}}'; fi
exit "${FAKE_CLAUDE_EXIT:-0}"
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CLAUDE_ARGS", argsPath)
	t.Setenv("FAKE_CLAUDE_STDIN", stdinPath)
	return executable, argsPath, stdinPath
}
