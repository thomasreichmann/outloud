package main

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/thomasreichmann/outloud/internal/coreaudio"
	"github.com/thomasreichmann/outloud/internal/macalert"
	"github.com/thomasreichmann/outloud/internal/playback"
	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/source"
)

// askLocally asks, in an alert, whether to pair with the PC called name.
func askLocally(name, code string, cancel <-chan struct{}) (bool, error) {
	return macalert.Ask(
		fmt.Sprintf("Pair “%s” with this Mac?", name),
		fmt.Sprintf("Allow it only if the PC shows the code %s.\n\n"+
			"Once paired, it can play its sound on this Mac and hear what this Mac plays.", code),
		"Allow", "Don't Allow", askFor, cancel)
}

func startDevice(src io.Reader, p *playout.Playout, bufferSize time.Duration) (output, error) {
	return playback.Open(src, p, outputRate, bufferSize)
}

// systemCapture is this Mac's own sound, for PCs that listen, with a note
// for the log; or nil and why not.
func systemCapture() (source.Source, string) {
	if err := coreaudio.Available(); err != nil {
		return nil, err.Error()
	}
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
