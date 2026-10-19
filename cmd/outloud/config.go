//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Config is the tray app's saved state, in %APPDATA%\Outloud.
type Config struct {
	// Receiver is the advertised name of the speaker to stream to. Empty
	// with an empty Address means streaming is off.
	Receiver string `json:"receiver"`
	// Address is host[:port]: the speaker's last known address, used when
	// it cannot be discovered, or the only way to reach it if Receiver is
	// empty.
	Address string `json:"address,omitempty"`
	// Device is the playback device to capture; empty follows the Windows
	// default.
	Device        string `json:"device,omitempty"`
	LatencyMs     int    `json:"latency_ms"`
	ForwardVolume bool   `json:"forward_volume"`
}

func (c Config) streaming() bool { return c.Receiver != "" || c.Address != "" }

func dataDir(env string) string {
	base := os.Getenv(env)
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "Outloud")
}

func configPath() string { return filepath.Join(dataDir("APPDATA"), "config.json") }

// loadConfig returns the saved config, or defaults and false on first run.
func loadConfig() (Config, bool) {
	// Following the Windows volume is off by default: that volume is usually
	// set for the PC's own speakers or headphones, and would leave the Mac
	// too quiet.
	c := Config{LatencyMs: 80}
	b, err := os.ReadFile(configPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, false
	}
	_ = json.Unmarshal(b, &c)
	return c, true
}

func saveConfig(c Config) error {
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := configPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, configPath())
}
