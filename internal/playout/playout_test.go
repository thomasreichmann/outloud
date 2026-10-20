package playout

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// sim drives a Playout with a simulated sender and device, each running on
// its own clock.
type sim struct {
	p                *Playout
	srcRate, outRate int
	// srcSpeed is how fast the sender's clock runs relative to the device's
	// (1.0003 is 300 ppm fast).
	srcSpeed float64
	// signal returns the sample for source frame i; nil means silence.
	signal func(i int) float32

	clock     time.Duration // simulated time, also the Playout's clock
	srcFrames int
	out       []float32 // left channel of everything read
}

const (
	writeChunk = 10 * time.Millisecond
	readChunk  = 5 * time.Millisecond
)

func (s *sim) run(d time.Duration) {
	const tick = 100 * time.Microsecond
	writeEvery := time.Duration(float64(writeChunk) / s.srcSpeed)
	nextWrite, nextRead := s.clock, s.clock
	for end := s.clock + d; s.clock < end; s.clock += tick {
		for s.clock >= nextWrite {
			s.write(int(writeChunk.Seconds() * float64(s.srcRate)))
			nextWrite += writeEvery
		}
		for s.clock >= nextRead {
			s.read(int(readChunk.Seconds() * float64(s.outRate)))
			nextRead += readChunk
		}
	}
}

func (s *sim) write(frames int) {
	pcm := make([]byte, 4*frames)
	for i := range frames {
		var v float32
		if s.signal != nil {
			v = s.signal(s.srcFrames + i)
		}
		x := uint16(int16(v * 32767))
		binary.LittleEndian.PutUint16(pcm[4*i:], x)
		binary.LittleEndian.PutUint16(pcm[4*i+2:], x)
	}
	s.srcFrames += frames
	s.p.Write(pcm)
}

func (s *sim) read(frames int) {
	b := make([]byte, 8*frames)
	s.p.Read(b)
	for i := range frames {
		s.out = append(s.out, math.Float32frombits(binary.LittleEndian.Uint32(b[8*i:])))
	}
}

func newSim(srcRate, outRate int, target time.Duration, srcSpeed float64) *sim {
	p := New(Config{OutputRate: outRate, Target: target})
	p.SetFormat(srcRate, 2)
	s := &sim{p: p, srcRate: srcRate, outRate: outRate, srcSpeed: srcSpeed}
	base := time.Unix(0, 0)
	p.now = func() time.Time { return base.Add(s.clock) }
	return s
}

func TestDriftIsCorrected(t *testing.T) {
	for _, ppm := range []float64{-300, 0, 300} {
		s := newSim(48000, 48000, 80*time.Millisecond, 1+ppm/1e6)
		s.run(10 * time.Minute)
		st := s.p.Stats()
		t.Logf("%+v ppm: buffered %v (target %v), correction %.1f ppm", ppm, st.Buffered, st.Target, st.DriftPPM)
		if st.Underruns != 0 || st.Skips != 0 {
			t.Errorf("%+v ppm: %d underruns, %d skips", ppm, st.Underruns, st.Skips)
		}
		if e := st.Buffered - st.Target; e < -3*time.Millisecond || e > 3*time.Millisecond {
			t.Errorf("%+v ppm: buffered %v, target %v", ppm, st.Buffered, st.Target)
		}
		if math.Abs(st.DriftPPM-ppm) > 30 {
			t.Errorf("%+v ppm: controller settled at %.1f ppm", ppm, st.DriftPPM)
		}
	}
}

func TestResamplesCleanly(t *testing.T) {
	const freq, amp = 1000.0, 0.5
	s := newSim(44100, 48000, 60*time.Millisecond, 1)
	s.signal = func(i int) float32 { return amp * float32(math.Sin(2*math.Pi*freq*float64(i)/44100)) }
	s.run(6 * time.Second)

	out := s.out[48000:] // skip priming
	var crossings int
	var sumSq, maxStep float64
	for i := 1; i < len(out); i++ {
		if (out[i-1] < 0) != (out[i] < 0) {
			crossings++
		}
		sumSq += float64(out[i]) * float64(out[i])
		maxStep = math.Max(maxStep, math.Abs(float64(out[i]-out[i-1])))
	}
	secs := float64(len(out)) / 48000
	if got := float64(crossings) / 2 / secs; math.Abs(got-freq) > 2 {
		t.Errorf("output frequency %.1f Hz, want %.0f", got, freq)
	}
	if rms := math.Sqrt(sumSq / float64(len(out))); math.Abs(rms-amp/math.Sqrt2) > 0.01 {
		t.Errorf("output RMS %.4f, want %.4f", rms, amp/math.Sqrt2)
	}
	// The steepest a 1 kHz sine at this amplitude gets is 2π·f/fs·amp per
	// sample; anything well above that is a click.
	if limit := 2 * math.Pi * freq / 48000 * amp * 1.1; maxStep > limit {
		t.Errorf("discontinuity: sample step %.4f exceeds %.4f", maxStep, limit)
	}
}

func TestIdleIsNotAnUnderrun(t *testing.T) {
	s := newSim(48000, 48000, 50*time.Millisecond, 1)
	s.run(time.Second)
	s.p.Idle()
	for range 40 {
		s.read(240)
	}
	if st := s.p.Stats(); st.State != Idle || st.Underruns != 0 {
		t.Fatalf("after idle: state %v, %d underruns", st.State, st.Underruns)
	}
}

func TestStarvationIsAnUnderrun(t *testing.T) {
	s := newSim(48000, 48000, 50*time.Millisecond, 1)
	s.run(time.Second)
	for range 40 {
		s.read(240)
	}
	if st := s.p.Stats(); st.State != Priming || st.Underruns != 1 {
		t.Fatalf("after starving: state %v, %d underruns", st.State, st.Underruns)
	}
	// Playback resumes once the target is buffered again.
	s.run(200 * time.Millisecond)
	if st := s.p.Stats(); st.State != Playing {
		t.Fatalf("did not resume: state %v", st.State)
	}
}

// TestUnderrunDoesNotClick checks that running dry and resuming fade the
// sound out and back in: no sample steps further than the tone itself does.
func TestUnderrunDoesNotClick(t *testing.T) {
	const freq, amp = 1000.0, 0.5
	s := newSim(48000, 48000, 50*time.Millisecond, 1)
	s.signal = func(i int) float32 { return amp * float32(math.Sin(2*math.Pi*freq*float64(i)/48000)) }
	s.run(time.Second)
	for range 40 {
		s.read(240)
	}
	s.run(time.Second)
	if st := s.p.Stats(); st.Underruns != 1 || st.State != Playing {
		t.Fatalf("%d underruns, state %v", st.Underruns, st.State)
	}
	out := s.out[48000/2:] // from well into the first playback
	var maxStep float64
	for i := 1; i < len(out); i++ {
		maxStep = math.Max(maxStep, math.Abs(float64(out[i]-out[i-1])))
	}
	if limit := 2 * math.Pi * freq / 48000 * amp * 1.1; maxStep > limit {
		t.Errorf("click: sample step %.4f exceeds %.4f", maxStep, limit)
	}
}

func TestTargetAdaptsToUnderruns(t *testing.T) {
	s := newSim(48000, 48000, 50*time.Millisecond, 1)
	s.run(time.Second)
	for range 40 {
		s.read(240)
	}
	st := s.p.Stats()
	if st.Underruns != 1 || st.Target != 70*time.Millisecond || st.Requested != 50*time.Millisecond {
		t.Fatalf("after an underrun: %d underruns, target %v (requested %v)", st.Underruns, st.Target, st.Requested)
	}
	// A clean network brings it back down, 10 ms a minute.
	s.run(150 * time.Second)
	st = s.p.Stats()
	if st.Target != 50*time.Millisecond || st.Underruns != 1 || st.Skips != 0 {
		t.Fatalf("after recovering: target %v, %d underruns, %d skips", st.Target, st.Underruns, st.Skips)
	}
	if e := st.Buffered - st.Target; e < -3*time.Millisecond || e > 3*time.Millisecond {
		t.Errorf("buffered %v, target %v", st.Buffered, st.Target)
	}
}

func TestBacklogIsTrimmed(t *testing.T) {
	s := newSim(48000, 48000, 80*time.Millisecond, 1)
	s.run(time.Second)
	s.write(48000) // a one-second burst after a stall
	st := s.p.Stats()
	if st.Skips != 1 {
		t.Fatalf("skips = %d, want 1", st.Skips)
	}
	if got := s.p.ring.n; got > s.p.target {
		t.Fatalf("ring holds %d frames after trim, target %d", got, s.p.target)
	}
}

func TestShortSoundPlaysOut(t *testing.T) {
	s := newSim(48000, 48000, 100*time.Millisecond, 1)
	s.signal = func(int) float32 { return 0.5 }
	s.write(960) // 20 ms, less than the target
	s.p.Idle()
	for range 20 {
		s.read(240)
	}
	var loud int
	for _, v := range s.out {
		if v > 0.25 {
			loud++
		}
	}
	if loud < 900 || loud > 960 {
		t.Errorf("%d loud frames played, want ~960", loud)
	}
	if st := s.p.State(); st != Idle {
		t.Errorf("state %v, want idle", st)
	}
}

func TestGainIsSmoothed(t *testing.T) {
	s := newSim(48000, 48000, 50*time.Millisecond, 1)
	s.signal = func(int) float32 { return 0.5 }
	s.run(time.Second)
	s.p.SetGain(0)
	s.out = nil
	s.run(300 * time.Millisecond)
	if first := s.out[0]; first < 0.45 {
		t.Errorf("gain jumped: first sample %.3f", first)
	}
	if last := s.out[len(s.out)-1]; math.Abs(float64(last)) > 1e-3 {
		t.Errorf("gain did not settle: last sample %.4f", last)
	}
}
