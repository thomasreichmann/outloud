package main

import (
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/thomasreichmann/outloud/internal/coreaudio"
	"github.com/thomasreichmann/outloud/internal/playback"
	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/source"
)

func startDevice(src io.Reader, p *playout.Playout, bufferSize time.Duration) (output, error) {
	return playback.Open(src, p, outputRate, bufferSize)
}

// systemCapture is this Mac's own sound, for PCs that listen.
func systemCapture() (source.Source, string) {
	return &coreaudio.Tap{}, "recording permission: " + coreaudio.Permission()
}

// computerName is the name shown in System Settings > General > Sharing.
func computerName() string {
	if n := scutil("ComputerName"); n != "" {
		return n
	}
	return hostName()
}

// localHostName is the name this Mac answers to as <name>.local.
func localHostName() string { return scutil("LocalHostName") }

func scutil(key string) string {
	out, err := exec.Command("/usr/sbin/scutil", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
