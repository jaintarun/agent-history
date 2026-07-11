package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ExpandPath("~/.local/share/agent-history/history.db")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "share", "agent-history", "history.db")
	if got != want {
		t.Fatalf("ExpandPath = %q, want %q", got, want)
	}
	literal := "relative/history.db"
	if got, err := ExpandPath(literal); err != nil || got != literal {
		t.Fatalf("ExpandPath literal = %q, %v", got, err)
	}
}
