// Package config owns application defaults and configuration loading.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Values contains command-level application settings.
type Values struct {
	Bind         string
	Database     string
	Config       string
	OpenBrowser  bool
	ScanInterval time.Duration
}

// File is the supported strict TOML configuration surface.
type File struct {
	Analysis Analysis `toml:"analysis"`
}

// Analysis contains initial analyzer settings for a new database.
type Analysis struct {
	Provider string `toml:"provider"`
	Model    string `toml:"model"`
	Auto     *bool  `toml:"auto"`
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

// Load decodes a config file. A missing file is equivalent to an empty file.
func Load(path string) (File, error) {
	var result File
	metadata, err := toml.DecodeFile(path, &result)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return File{}, fmt.Errorf("decode config: %w", err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) != 0 {
		return File{}, fmt.Errorf("unknown config key %q", undecoded[0].String())
	}
	return result, nil
}
