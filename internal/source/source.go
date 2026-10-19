// Package source defines what the sender streams from: a capture backend
// emits Events, and the sender turns them into protocol messages.
package source

import (
	"context"
	"errors"
	"math"
	"time"
)

// Errors in Status events that the side playing the audio can explain to
// its user; sources wrap them.
var (
	// ErrNeedsPermission means the operating system is asking the person
	// at this computer whether Outloud may record its sound. Capture waits
	// for the answer.
	ErrNeedsPermission = errors.New("waiting for permission to record this computer's sound")
	// ErrDenied means Outloud may not record this computer's sound.
	ErrDenied = errors.New("not allowed to record this computer's sound")
)

type Kind int

const (
	// Audio carries interleaved stereo s16 samples in Samples.
	Audio Kind = iota
	// Format announces the sample rate of the Audio that follows and the
	// name of the device being captured.
	Format
	// Idle means the source stopped producing sound.
	Idle
	// Volume carries the gain to apply on the receiver.
	Volume
	// Status reports, in Err, why capture is not running. The source keeps
	// retrying.
	Status
)

type Event struct {
	Kind    Kind
	Samples []int16
	Rate    int
	Device  string
	Gain    float32
	Err     error
}

// Source produces audio until its context is cancelled. Run calls emit from
// a single goroutine and must not block for long inside emit's caller.
type Source interface {
	Run(ctx context.Context, emit func(Event)) error
}

// Tone is a synthetic source: half-second beeps of a sine, one per second.
// It exercises the whole path to a receiver without touching sound cards.
type Tone struct {
	Freq float64
	Gain float64
}

func (t Tone) Run(ctx context.Context, emit func(Event)) error {
	const rate, chunk = 48000, 480
	emit(Event{Kind: Format, Rate: rate, Device: "test tone"})
	emit(Event{Kind: Volume, Gain: 1})
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	start := time.Now()
	var n int // frames emitted
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-tick.C:
			// Emit by elapsed time rather than per tick so ticker jitter
			// does not change the rate.
			for due := int(now.Sub(start).Seconds() * rate); n+chunk <= due; n += chunk {
				s := make([]int16, 2*chunk)
				for i := range chunk {
					f := n + i
					if f%rate < rate/2 {
						v := int16(t.Gain * 32767 * math.Sin(2*math.Pi*t.Freq*float64(f)/rate))
						s[2*i], s[2*i+1] = v, v
					}
				}
				emit(Event{Kind: Audio, Samples: s})
			}
		}
	}
}
