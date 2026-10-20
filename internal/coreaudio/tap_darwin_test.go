package coreaudio

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

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

// TestPlayers checks that the watch is told when afplay starts playing and
// when it stops.
func TestPlayers(t *testing.T) {
	needTap(t)
	w, err := watchPlayers()
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	// players lists the PIDs of the processes that play, and whether one
	// besides this one does.
	players := func() (map[int32]bool, bool) {
		another, err := w.playing()
		if err != nil {
			t.Fatal(err)
		}
		pids := map[int32]bool{}
		for obj, pid := range w.procs {
			var running uint32
			if getProperty(obj, propRunningOutput, nil, 0, unsafe.Pointer(&running), 4) == nil && running != 0 {
				pids[pid] = true
			}
		}
		return pids, another
	}
	pids, _ := players()
	t.Logf("%d processes use Core Audio; these play: %v; permission %s", len(w.procs), pids, Permission())

	afplay := exec.Command("/usr/bin/afplay", "/System/Library/Sounds/Ping.aiff")
	start := time.Now()
	if err := afplay.Start(); err != nil {
		t.Fatal(err)
	}
	pid := int32(afplay.Process.Pid)
	// await waits for cond, checking it only when the watch says something
	// changed.
	await := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for !cond() {
			select {
			case <-w.changes():
			case <-deadline:
				t.Fatalf("not told of %s", what)
			}
		}
		t.Logf("told of %s %v after starting afplay", what, time.Since(start).Round(time.Millisecond))
	}
	await("afplay playing", func() bool { pids, another := players(); return pids[pid] && another })
	if err := afplay.Wait(); err != nil {
		t.Fatal(err)
	}
	await("afplay done", func() bool { pids, _ := players(); return !pids[pid] })
}

// TestLoad checks, on any Mac that can capture, that every function the
// tap uses is there, and that the watch for processes that play works. It
// plays nothing and asks for nothing, so it runs on build machines too, of
// both kinds.
func TestLoad(t *testing.T) {
	v, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		t.Fatal(err)
	}
	var major, minor int
	fmt.Sscanf(v, "%d.%d", &major, &minor)
	err = load()
	if major < 14 || major == 14 && minor < 2 {
		if err == nil {
			t.Errorf("macOS %s can capture?", v)
		}
		t.Skipf("macOS %s cannot capture: %v", v, err)
	}
	if err != nil {
		t.Fatalf("macOS %s: %v", v, err)
	}
	w, err := watchPlayers()
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	playing, err := w.playing()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("macOS %s on %s: %d processes use Core Audio, another plays: %v, permission %s",
		v, runtime.GOARCH, len(w.procs), playing, Permission())
}

// tone is a quiet sine, as a Player's source, counting the frames read.
type tone struct {
	frames atomic.Int64
	phase  float64
}

func (s *tone) Read(b []byte) (int, error) {
	n := len(b) / 8
	for i := range n {
		v := math.Float32bits(float32(0.05 * math.Sin(s.phase)))
		s.phase += 2 * math.Pi * 440 / 48000
		binary.LittleEndian.PutUint32(b[8*i:], v)
		binary.LittleEndian.PutUint32(b[8*i+4:], v)
	}
	s.frames.Add(int64(n))
	return 8 * n, nil
}

// TestPlayer plays a quiet tone for three seconds, through the device's own
// buffer, and checks that the device was fed all along without missing a
// cycle; then that it stops and starts again.
func TestPlayer(t *testing.T) {
	needTap(t)
	src := &tone{}
	p, err := NewPlayer(src, 48000, 256)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Sleep()
	lat, err := p.Latency()
	t.Logf("latency %v (%v)", lat, err)
	time.Sleep(3 * time.Second)
	frames := src.frames.Load()
	t.Logf("%d frames in 3 s, %d overloads", frames, p.Overloads())
	if frames < 3*48000*9/10 || frames > 3*48000*11/10 {
		t.Errorf("read %d frames in 3 s, want about %d", frames, 3*48000)
	}
	if p.Overloads() != 0 {
		t.Errorf("%d overloads", p.Overloads())
	}
	p.Sleep()
	time.Sleep(200 * time.Millisecond)
	before := src.frames.Load()
	time.Sleep(300 * time.Millisecond)
	if after := src.frames.Load(); after != before {
		t.Errorf("read %d frames while asleep", after-before)
	}
	p.Wake()
	time.Sleep(300 * time.Millisecond)
	if src.frames.Load() == before {
		t.Error("nothing read after waking")
	}
}

// TestOutput reads the default output's name and mute state. It plays
// nothing, so it runs everywhere on a Mac.
func TestOutput(t *testing.T) {
	if err := load(); err != nil {
		t.Skip(err)
	}
	name, muted, err := Output()
	if err != nil {
		t.Skip(err) // a build machine may have no output device
	}
	if name == "" {
		t.Error("the default output has no name")
	}
	t.Logf("default output %q, muted %v", name, muted)
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
