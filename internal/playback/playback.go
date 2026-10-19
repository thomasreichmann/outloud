//go:build darwin || windows

// Package playback plays a Playout through the system's default output
// device.
package playback

import (
	"io"
	"runtime"
	"sync"
	"time"

	"github.com/ebitengine/oto/v3"

	"github.com/thomasreichmann/outloud/internal/playout"
)

// Device plays through CoreAudio's or WASAPI's default output. A process can
// open only one.
type Device struct {
	ctx    *oto.Context
	player *oto.Player // must stay reachable to keep playing
	p      *playout.Playout

	mu     sync.Mutex
	asleep bool
}

// Open plays src, which is p or reads from it, at rate Hz in stereo float.
// bufferSize is the device buffer: larger survives a busier system, smaller
// adds less latency.
func Open(src io.Reader, p *playout.Playout, rate int, bufferSize time.Duration) (*Device, error) {
	ctx, ready, err := oto.NewContext(&oto.NewContextOptions{
		SampleRate:      rate,
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
	// oto fills each device buffer only from what the player has already
	// read ahead, padding with silence if that falls short. So the read-ahead
	// must hold a whole device buffer; two leave room for the refill. On
	// macOS bufferSize is split over four AudioQueue buffers; WASAPI fills
	// all of it at once. The default, half a second, would add that much
	// latency.
	deviceBuffer := int(bufferSize.Seconds()*float64(rate)) * 8
	if runtime.GOOS == "darwin" {
		deviceBuffer = int(bufferSize.Seconds()*float64(rate)) / 4 * 8
	}
	player.SetBufferSize(2 * max(deviceBuffer, 8*rate/1000))
	player.Play()
	return &Device{ctx: ctx, player: player, p: p}, nil
}

// Wake makes sure the device is running; audio is arriving.
func (d *Device) Wake() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.asleep {
		d.asleep = false
		_ = d.ctx.Resume()
	}
}

// Sleep pauses the device if the playout is still idle. A running audio
// queue keeps macOS from sleeping even when it only plays silence.
func (d *Device) Sleep() {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Checked under d.mu: audio arriving now calls Wake, which waits for us
	// and then resumes.
	if !d.asleep && d.p.State() == playout.Idle {
		d.asleep = true
		_ = d.ctx.Suspend()
	}
}

func (d *Device) Err() error { return d.ctx.Err() }
