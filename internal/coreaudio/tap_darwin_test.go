package coreaudio

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/thomasreichmann/outloud/internal/source"
)

// These tests need a Mac with an output device, and play a sound on it.
// Where macOS has not decided on the recording permission, starting a tap
// can wait for someone to answer its prompt, so they only run on request.
func needTap(t *testing.T) {
	t.Helper()
	if os.Getenv("OUTLOUD_TAP_TEST") == "" {
		t.Skip("set OUTLOUD_TAP_TEST=1 to test system audio capture on this Mac")
	}
	if err := load(); err != nil {
		t.Skip(err)
	}
}

// TestTap runs the tap for two seconds while macOS plays a sound. Whether
// any of it is heard depends on the privacy permission, so only the setup
// is checked; the rest is logged.
func TestTap(t *testing.T) {
	needTap(t)
	t.Logf("permission: %s", Permission())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = exec.CommandContext(ctx, "/usr/bin/afplay", "/System/Library/Sounds/Ping.aiff").Run()
	}()

	var format source.Event
	var audio, frames, idle int
	var errs []error
	tap := &Tap{}
	if err := tap.Run(ctx, func(e source.Event) {
		switch e.Kind {
		case source.Format:
			format = e
		case source.Audio:
			audio++
			frames += len(e.Samples) / 2
		case source.Idle:
			idle++
		case source.Status:
			errs = append(errs, e.Err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	for _, err := range errs {
		t.Errorf("status: %v", err)
	}
	if format.Rate == 0 {
		t.Fatal("no format announced")
	}
	t.Logf("format %d Hz from %q; %d audio packets, %d frames, %d idles", format.Rate, format.Device, audio, frames, idle)
}

// TestBuffers checks that the IOProc delivers buffers at all, silent or not.
func TestBuffers(t *testing.T) {
	needTap(t)
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	t.Logf("tap format: %d Hz, %d ch, non-interleaved %v", s.rate, s.channels, s.nonInterleaved)
	go func() { _ = exec.Command("/usr/bin/afplay", "/System/Library/Sounds/Ping.aiff").Run() }()
	var chunks, bytes int
	var peak float32
	conv := source.NewConverter(source.Float32, s.channels, 0)
	deadline := time.After(1500 * time.Millisecond)
	for {
		select {
		case b := <-s.chunks:
			chunks++
			bytes += len(b)
			out, _ := conv.Convert(nil, b, len(b)/conv.FrameBytes())
			for _, v := range out {
				peak = max(peak, float32(max(v, -v))/32768)
			}
		case <-deadline:
			t.Logf("%d buffers, %d bytes, peak %.4f", chunks, bytes, peak)
			if chunks == 0 {
				t.Error("no buffers delivered")
			}
			return
		}
	}
}
