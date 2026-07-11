// Package config owns application defaults and configuration loading.
package config

import "time"

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
