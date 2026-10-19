//go:build !darwin

package main

import (
	"errors"
	"io"
	"time"

	"github.com/thomasreichmann/outloud/internal/playout"
)

func startDevice(io.Reader, *playout.Playout, time.Duration) (output, error) {
	return nil, errors.New("playback is only implemented on macOS; use -null to test")
}

func computerName() string { return hostName() }

func localHostName() string { return hostName() }

func install([]string) error { return errors.New("install is only implemented on macOS") }

func uninstall() error { return errors.New("uninstall is only implemented on macOS") }
