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

func TestLoadAnalysisConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := "[analysis]\nprovider = \"codex-cli\"\nmodel = \"cheap-model\"\nauto = false\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Analysis.Provider != "codex-cli" || loaded.Analysis.Model != "cheap-model" || loaded.Analysis.Auto == nil || *loaded.Analysis.Auto {
		t.Fatalf("loaded config = %#v", loaded)
	}
}

func TestLoadMissingConfigUsesZeroValuesAndRejectsUnknownFields(t *testing.T) {
	loaded, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil || loaded.Analysis.Auto != nil {
		t.Fatalf("missing config = %#v, %v", loaded, err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[analysis]\nsecret = \"no\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown config field was accepted")
	}
}
