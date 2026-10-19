package main

import (
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ebitengine/oto/v3"

	"github.com/thomasreichmann/outloud/internal/playout"
)

// device plays through CoreAudio's default output.
type device struct {
	ctx    *oto.Context
	player *oto.Player // must stay reachable to keep playing
	p      *playout.Playout

	mu     sync.Mutex
	asleep bool
}

func startDevice(src io.Reader, p *playout.Playout, bufferSize time.Duration) (output, error) {
	ctx, ready, err := oto.NewContext(&oto.NewContextOptions{
		SampleRate:      outputRate,
		ChannelCount:    2,
		Format:          oto.FormatFloat32LE,
		BufferSize:      bufferSize,
		ApplicationName: "Outloud",
	})
	if err != nil {
		return nil, err
	}
	<-ready
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	player := ctx.NewPlayer(src)
	// oto fills each of its four AudioQueue buffers only from what the player
	// has already read ahead, padding with silence if that falls short. So
	// the read-ahead must hold a whole queue buffer; two leave room for the
	// refill. The default, half a second, would add that much latency.
	queueBuffer := int(bufferSize.Seconds()*outputRate) / 4 * 8
	player.SetBufferSize(2 * max(queueBuffer, 8*outputRate/1000))
	player.Play()
	return &device{ctx: ctx, player: player, p: p}, nil
}

func (d *device) Wake() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.asleep {
		d.asleep = false
		_ = d.ctx.Resume()
	}
}

// Sleep pauses the audio queue. A running queue keeps macOS from sleeping
// even when it only plays silence.
func (d *device) Sleep() {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Checked under d.mu: audio arriving now calls Wake, which waits for us
	// and then resumes.
	if !d.asleep && d.p.State() == playout.Idle {
		d.asleep = true
		_ = d.ctx.Suspend()
	}
}

func (d *device) Err() error { return d.ctx.Err() }

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
