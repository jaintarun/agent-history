// Package config owns application defaults and configuration loading.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Values contains command-level application settings.
type Values struct {
	Bind         string
	Database     string
	Config       string
	OpenBrowser  bool
	ScanInterval time.Duration
}

// Defaults returns settings used when no file or command flag overrides them.
func Defaults() Values {
	return Values{
		Bind:         "127.0.0.1:0",
		Database:     "~/.local/share/agent-history/history.db",
		Config:       "~/.config/agent-history/config.toml",
		OpenBrowser:  true,
		ScanInterval: 15 * time.Minute,
	}
}

// ExpandPath expands a leading home-directory marker without interpreting any
// other shell syntax.
func ExpandPath(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}
