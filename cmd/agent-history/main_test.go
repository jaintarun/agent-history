package main

import (
	"bytes"
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
