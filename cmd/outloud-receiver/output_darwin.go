package main

import (
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
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

// startDevice plays src through the default output, which asks for buffer's
// worth at a time, straight from the playout. If that cannot start, an audio
// queue, which keeps more buffered, plays it instead.
func startDevice(src io.Reader, p *playout.Playout, buffer time.Duration) (output, error) {
	pl, err := coreaudio.NewPlayer(src, outputRate, int(buffer.Seconds()*outputRate))
	if err != nil {
		log.Printf("audio output: %v; playing through an audio queue instead", err)
		return playback.Open(src, p, outputRate, max(buffer, 40*time.Millisecond))
	}
	if lat, err := pl.Latency(); err == nil {
		log.Printf("audio output: %v from the buffer to the speakers", lat.Round(time.Millisecond))
	}
	return &player{pl: pl, p: p}, nil
}

// player is a coreaudio.Player as an output.
type player struct {
	pl *coreaudio.Player
	p  *playout.Playout
	mu sync.Mutex
}

func (o *player) Wake() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pl.Wake()
}

// Sleep stops the output if the playout is still idle. Audio that arrives
// meanwhile calls Wake, which waits for Sleep and then starts it again.
func (o *player) Sleep() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.p.State() == playout.Idle {
		o.pl.Sleep()
	}
}

func (o *player) Overloads() int64 { return o.pl.Overloads() }

func (o *player) Err() error { return nil }

// systemCapture is this Mac's own sound, for PCs that listen, with a note
// for the log; or nil and why not.
func systemCapture() (source.Source, string) {
	if err := coreaudio.Available(); err != nil {
		return nil, err.Error()
	}
	return &coreaudio.Tap{}, "recording permission: " + coreaudio.Permission()
}

// systemOutput is the default output device, and whether it is muted.
func systemOutput() (string, bool, error) { return coreaudio.Output() }

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
