package wasapi

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ebitengine/oto/v3"

	"github.com/thomasreichmann/outloud/internal/source"
)

// The probes are faint tones above what most people hear and what music
// holds: one played by the test itself, one by another process.
const (
	ownProbe   = 18500
	otherProbe = 17500
	probeGain  = 0.003 // -50 dBFS
)

// probe is an endless faint sine in stereo float.
type probe struct {
	freq float64
	n    int
}

func (p *probe) Read(b []byte) (int, error) {
	frames := len(b) / 8
	for i := range frames {
		v := float32(probeGain * math.Sin(2*math.Pi*p.freq*float64(p.n)/48000))
		p.n++
		binary.LittleEndian.PutUint32(b[8*i:], math.Float32bits(v))
		binary.LittleEndian.PutUint32(b[8*i+4:], math.Float32bits(v))
	}
	return frames * 8, nil
}

func playProbe(t *testing.T, freq float64) func() {
	t.Helper()
	ctx, ready, err := oto.NewContext(&oto.NewContextOptions{SampleRate: 48000, ChannelCount: 2, Format: oto.FormatFloat32LE})
	if err != nil {
		t.Fatal(err)
	}
	<-ready
	player := ctx.NewPlayer(&probe{freq: freq})
	player.Play()
	return func() { player.Close() }
}

// probeLevels measures the probe frequencies in captured audio with the
// Goertzel algorithm, in blocks of 1024 frames.
type probeLevels struct {
	mu     sync.Mutex
	rate   int
	device string
	block  []float64
	sum    map[float64]float64
	blocks int
}

func (pl *probeLevels) add(e source.Event) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	switch e.Kind {
	case source.Format:
		pl.rate, pl.device = e.Rate, e.Device
	case source.Audio:
		if pl.sum == nil {
			pl.sum = map[float64]float64{}
		}
		for i := 0; i < len(e.Samples); i += 2 {
			pl.block = append(pl.block, float64(e.Samples[i])/32768)
			if len(pl.block) == 1024 {
				for _, f := range []float64{ownProbe, otherProbe} {
					pl.sum[f] += goertzel(pl.block, f/float64(pl.rate))
				}
				pl.blocks++
				pl.block = pl.block[:0]
			}
		}
	}
}

// dB is the mean amplitude of frequency f in dBFS, or -inf without audio:
// sources send nothing while all is silent.
func (pl *probeLevels) dB(f float64) float64 {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.blocks == 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(pl.sum[f]/float64(pl.blocks)+1e-12)
}

// goertzel returns the amplitude of frequency f (cycles per sample) in x.
func goertzel(x []float64, f float64) float64 {
	w := 2 * math.Pi * f
	c := 2 * math.Cos(w)
	var s1, s2 float64
	for i, v := range x {
		// A Hann window keeps louder neighbouring frequencies out.
		v *= 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(len(x)-1))
		s1, s2 = v+c*s1-s2, s1
	}
	re := s1 - s2*math.Cos(w)
	im := s2 * math.Sin(w)
	return 2 * math.Hypot(re, im) / (float64(len(x)) / 2) // /2: the window's gain
}

// TestOtherProbe is not a test: it plays the other probe for a while, as a
// separate process for TestExcludeSelf to hear.
func TestOtherProbe(t *testing.T) {
	if os.Getenv("OUTLOUD_OTHER_PROBE") == "" {
		t.Skip("set OUTLOUD_OTHER_PROBE=1 to play the other probe for TestExcludeSelf")
	}
	defer playProbe(t, otherProbe)()
	time.Sleep(8 * time.Second)
}

// TestExcludeSwitch turns leaving this process out on and off during a
// capture, as turning hearing on and off in the tray app does.
func TestExcludeSwitch(t *testing.T) {
	if os.Getenv("OUTLOUD_LOOPBACK_TEST") == "" {
		t.Skip("set OUTLOUD_LOOPBACK_TEST=1 to test on this PC's default playback device")
	}
	var mu sync.Mutex
	exclude := false
	set := func(v bool) {
		mu.Lock()
		exclude = v
		mu.Unlock()
	}
	l := &Loopback{ExcludeSelf: func() bool {
		mu.Lock()
		defer mu.Unlock()
		return exclude
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		time.Sleep(1500 * time.Millisecond)
		set(true)
		time.Sleep(2 * time.Second)
		set(false)
	}()
	var formats []string
	l.Run(ctx, func(e source.Event) {
		switch e.Kind {
		case source.Format:
			formats = append(formats, e.Device)
		case source.Status:
			t.Errorf("status: %v", e.Err)
		}
	})
	t.Logf("formats: %q", formats)
	if len(formats) != 3 || strings.Contains(formats[0], "without") || !strings.Contains(formats[1], "without") || strings.Contains(formats[2], "without") {
		t.Errorf("want plain, excluding, plain; got %q", formats)
	}
}

// TestExcludeSelf plays a faint tone and captures the default device both
// ways: the tone must be there in plain loopback and gone when this
// process's own sound is left out. With OUTLOUD_OTHER_PROBE=1, another
// process plays a second tone (TestOtherProbe) which must stay.
func TestExcludeSelf(t *testing.T) {
	if os.Getenv("OUTLOUD_LOOPBACK_TEST") == "" {
		t.Skip("set OUTLOUD_LOOPBACK_TEST=1 to test on this PC's default playback device")
	}
	other := os.Getenv("OUTLOUD_OTHER_PROBE") != ""
	defer playProbe(t, ownProbe)()

	run := func(exclude bool, pl *probeLevels, errs chan<- error) {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		l := &Loopback{ExcludeSelf: func() bool { return exclude }}
		errs <- l.Run(c, func(e source.Event) {
			if e.Kind == source.Status {
				t.Errorf("exclude %v: %v", exclude, e.Err)
			}
			pl.add(e)
		})
	}
	var plain, excluded probeLevels
	errs := make(chan error, 2)
	go run(false, &plain, errs)
	go run(true, &excluded, errs)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []*probeLevels{&plain, &excluded} {
		t.Logf("%q: own probe %.1f dBFS, other probe %.1f dBFS, over %d blocks",
			c.device, c.dB(ownProbe), c.dB(otherProbe), c.blocks)
	}
	if plain.dB(ownProbe) < -60 {
		t.Fatalf("plain loopback did not hear this process (%.1f dBFS)", plain.dB(ownProbe))
	}
	if !strings.Contains(excluded.device, "without Outloud's own sound") {
		t.Fatalf("process loopback did not start: capturing %q", excluded.device)
	}
	if excluded.dB(ownProbe) > plain.dB(ownProbe)-20 {
		t.Errorf("this process is still in the capture that excludes it: %.1f dBFS", excluded.dB(ownProbe))
	}
	if other && excluded.dB(otherProbe) < -60 {
		t.Errorf("the capture that excludes this process lost the other process too: %.1f dBFS", excluded.dB(otherProbe))
	}
}
