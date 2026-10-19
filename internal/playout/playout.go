// Package playout turns a jittery stream of network audio into the steady
// stream an audio device pulls from.
//
// Two clocks are involved: the sender's sound card produces samples at its
// rate, and the receiver's sound card consumes them at its own. Even when both
// nominally run at 48 kHz they differ by tens of parts per million, which is
// enough to drain or overflow any fixed buffer within minutes. Playout keeps a
// target amount of audio buffered and resamples by a tiny, continuously
// adjusted ratio so the buffer level stays put: a PI controller watches the
// smoothed buffer level and nudges the resampling step.
//
// Writes come from the network goroutine; Read is called by the device's pull
// loop. Both are safe to call concurrently.
package playout

import (
	"encoding/binary"
	"math"
	"sync"
	"time"
)

type State int

const (
	// Idle means there is nothing to play; output is silence.
	Idle State = iota
	// Priming means audio is arriving and is being buffered up to the
	// target before playback (re)starts.
	Priming
	// Playing means samples are flowing out.
	Playing
)

func (s State) String() string {
	switch s {
	case Idle:
		return "idle"
	case Priming:
		return "buffering"
	case Playing:
		return "playing"
	}
	return "unknown"
}

const (
	// Controller tuning. With kp = 0.1/s a 10 ms buffer error speeds up or
	// slows down playback by 0.1 % (under 2 cents of pitch), and ki is chosen
	// for critical damping. maxAdj bounds the correction to 0.2 %, far more
	// than any real clock mismatch.
	kp     = 0.1
	ki     = kp * kp / 4
	maxAdj = 0.002
	// fillSmoothing is the time constant of the buffer-level average. Packets
	// arrive in ~10 ms bursts, so the raw level is a sawtooth.
	fillSmoothing = 1.0
	// gainSmoothing is the time constant of volume changes, so they never
	// click.
	gainSmoothing = 0.02

	minTarget = 10 * time.Millisecond
	maxTarget = time.Second
	// minSlack is the least amount of audio above the target tolerated
	// before the oldest audio is dropped to restore the latency.
	minSlack = 60 * time.Millisecond
	// ringSeconds is the buffer capacity; it must exceed target + slack.
	ringSeconds = 3

	// The target adapts to the network: every underrun raises it by a step,
	// up to maxGrowth above what was asked for, and after each quiet
	// shrinkEvery it creeps back down by shrinkStep.
	minGrowStep = 20 * time.Millisecond
	maxGrowth   = 250 * time.Millisecond
	shrinkEvery = time.Minute
	shrinkStep  = 10 * time.Millisecond
)

type Config struct {
	// OutputRate is the sample rate Read produces.
	OutputRate int
	// Target is the amount of audio to keep buffered.
	Target time.Duration
	// OnWake, if set, is called when audio arrives while Idle. It runs on
	// the writer's goroutine with no locks held.
	OnWake func()
}

// Stats is a snapshot of the playout state.
type Stats struct {
	State      State
	SampleRate int
	Buffered   time.Duration // smoothed while playing, instantaneous otherwise
	Target     time.Duration // current target, raised after underruns
	Requested  time.Duration // target as configured
	DriftPPM   float64       // current resampling correction
	Underruns  int           // times playback ran dry while the source was active
	Skips      int           // times audio was dropped to restore the latency
}

type Playout struct {
	mu sync.Mutex

	outRate    float64
	baseTarget time.Duration // as configured
	targetTime time.Duration // in effect
	lastGrow   time.Time     // last time targetTime changed
	onWake     func()

	srcRate     int
	srcChannels int
	target      int // frames at srcRate
	maxFill     int // frames at srcRate
	ring        ring
	state       State
	sourceIdle  bool

	// Resampler. hist holds four consecutive source frames; output is
	// interpolated between hist[1] and hist[2] at fractional position pos.
	pos  float64
	step float64
	hist [4][2]float32

	// Drift controller. The ring level is a sawtooth: it jumps up by a
	// packet on every write and down by a chunk on every read. Sampling it at
	// read times would alias the write sawtooth into a slow beat, since the
	// two run on different clocks, so the level is integrated over time
	// instead and the controller works on the time average.
	now   func() time.Time
	lastT time.Time
	area  float64 // ∫ level dt since the last control update, frame·seconds
	areaT float64 // seconds covered by area
	fill  float64 // smoothed ring level in frames
	integ float64
	adj   float64

	gain, gainTarget, gainCoef float32

	underruns, skips int
}

func New(cfg Config) *Playout {
	p := &Playout{
		outRate:    float64(cfg.OutputRate),
		baseTarget: clampTarget(cfg.Target),
		targetTime: clampTarget(cfg.Target),
		onWake:     cfg.OnWake,
		now:        time.Now,
		gain:       1,
		gainTarget: 1,
		gainCoef:   float32(1 - math.Exp(-1/(gainSmoothing*float64(cfg.OutputRate)))),
	}
	return p
}

func clampTarget(d time.Duration) time.Duration {
	return min(max(d, minTarget), maxTarget)
}

// SetFormat declares the format of subsequent writes. Changing it discards
// buffered audio.
func (p *Playout) SetFormat(rate, channels int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rate == p.srcRate && channels == p.srcChannels {
		return
	}
	p.srcRate, p.srcChannels = rate, channels
	p.ring.reset(rate * ringSeconds)
	p.integ, p.adj = 0, 0
	p.applyTarget()
	p.state = Idle
}

// SetTarget changes the amount of audio kept buffered.
func (p *Playout) SetTarget(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.baseTarget = clampTarget(d)
	p.targetTime = p.baseTarget
	p.applyTarget()
}

// grow raises the target after an underrun.
func (p *Playout) grow() {
	step := max(p.baseTarget/4, minGrowStep)
	p.targetTime = min(p.targetTime+step, p.baseTarget+maxGrowth, maxTarget)
	p.lastGrow = p.now()
	p.applyTarget()
}

// relax lowers a raised target once the network has behaved for a while.
// The drift controller then drains the difference inaudibly.
func (p *Playout) relax() {
	if p.targetTime > p.baseTarget && p.now().Sub(p.lastGrow) >= shrinkEvery {
		p.targetTime = max(p.targetTime-shrinkStep, p.baseTarget)
		p.lastGrow = p.now()
		p.applyTarget()
	}
}

func (p *Playout) applyTarget() {
	p.target = int(p.targetTime.Seconds() * float64(p.srcRate))
	slack := max(p.targetTime, minSlack)
	p.maxFill = p.target + int(slack.Seconds()*float64(p.srcRate))
}

// Reset drops all buffered audio and forgets the drift estimate, for a new
// sender.
func (p *Playout) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ring.clear()
	p.state = Idle
	p.sourceIdle = false
	p.integ, p.adj = 0, 0
	p.gainTarget = 1
	p.underruns, p.skips = 0, 0
	p.targetTime = p.baseTarget
	p.applyTarget()
}

// SetGain sets the linear output gain; changes are smoothed.
func (p *Playout) SetGain(g float32) {
	p.mu.Lock()
	p.gainTarget = g
	p.mu.Unlock()
}

// Write appends interleaved s16le samples in the current format.
func (p *Playout) Write(pcm []byte) {
	p.mu.Lock()
	if p.srcRate == 0 {
		p.mu.Unlock()
		return
	}
	p.integrate()
	ch := p.srcChannels
	for i := 0; i+2*ch <= len(pcm); i += 2 * ch {
		l := float32(int16(binary.LittleEndian.Uint16(pcm[i:]))) / 32768
		r := l
		if ch == 2 {
			r = float32(int16(binary.LittleEndian.Uint16(pcm[i+2:]))) / 32768
		}
		p.ring.push(l, r)
	}
	p.sourceIdle = false
	wake := p.state == Idle
	if wake {
		p.state = Priming
	}
	if p.ring.n > p.maxFill {
		// A network stall just ended and delivered its backlog at once.
		// Drop the oldest audio rather than carry the extra latency.
		p.ring.discard(p.ring.n - p.target)
		p.fill = float64(p.ring.n)
		if p.state == Playing {
			p.skips++
		}
	}
	p.mu.Unlock()
	if wake && p.onWake != nil {
		p.onWake()
	}
}

// Idle tells the playout that the source has stopped producing sound, so
// running dry afterwards is expected rather than an underrun.
func (p *Playout) Idle() {
	p.mu.Lock()
	p.sourceIdle = true
	p.mu.Unlock()
}

// Read fills b with interleaved float32le stereo at the output rate. It never
// blocks and always fills whole frames, playing silence when there is
// nothing to play.
func (p *Playout) Read(b []byte) (int, error) {
	frames := len(b) / 8
	p.mu.Lock()
	defer p.mu.Unlock()
	p.integrate()
	p.updateControl()
	for i := range frames {
		var l, r float32
		if p.state == Priming {
			switch {
			case p.ring.n >= p.target:
				p.startPlaying()
			case p.sourceIdle && p.ring.n > 0:
				// A sound shorter than the target: play out what there is.
				p.startPlaying()
			case p.sourceIdle:
				p.state = Idle
			}
		}
		if p.state == Playing {
			var ok bool
			if l, r, ok = p.next(); !ok {
				if p.sourceIdle {
					p.state = Idle
				} else {
					p.state = Priming
					p.underruns++
					p.grow()
				}
			}
		}
		p.gain += (p.gainTarget - p.gain) * p.gainCoef
		binary.LittleEndian.PutUint32(b[8*i:], math.Float32bits(l*p.gain))
		binary.LittleEndian.PutUint32(b[8*i+4:], math.Float32bits(r*p.gain))
	}
	return frames * 8, nil
}

func (p *Playout) startPlaying() {
	p.state = Playing
	p.hist = [4][2]float32{}
	p.pos = 3 // the first next() pulls three frames, so output starts exactly on the first one
	p.fill = float64(p.ring.n)
	p.area, p.areaT = 0, 0
	p.step = float64(p.srcRate) / p.outRate * (1 + p.adj)
}

// integrate accumulates the ring level over the time since the last write or
// read. It must run before every change to the ring level.
func (p *Playout) integrate() {
	t := p.now()
	if !p.lastT.IsZero() {
		dt := t.Sub(p.lastT).Seconds()
		p.area += float64(p.ring.n) * dt
		p.areaT += dt
	}
	p.lastT = t
}

// next produces one output frame, or reports that the ring ran dry.
func (p *Playout) next() (float32, float32, bool) {
	for p.pos >= 1 {
		l, r, ok := p.ring.pop()
		if !ok {
			return 0, 0, false
		}
		p.hist[0], p.hist[1], p.hist[2] = p.hist[1], p.hist[2], p.hist[3]
		p.hist[3] = [2]float32{l, r}
		p.pos--
	}
	t := float32(p.pos)
	h := &p.hist
	l := hermite(h[0][0], h[1][0], h[2][0], h[3][0], t)
	r := hermite(h[0][1], h[1][1], h[2][1], h[3][1], t)
	p.pos += p.step
	return l, r, true
}

// hermite is 4-point, 3rd-order Hermite interpolation between x0 and x1.
func hermite(xm1, x0, x1, x2, t float32) float32 {
	c1 := 0.5 * (x1 - xm1)
	c2 := xm1 - 2.5*x0 + 2*x1 - 0.5*x2
	c3 := 0.5*(x2-xm1) + 1.5*(x0-x1)
	return ((c3*t+c2)*t+c1)*t + x0
}

// updateControl runs the drift controller once per Read.
func (p *Playout) updateControl() {
	dt := p.areaT
	if p.state != Playing || dt <= 0 {
		p.area, p.areaT = 0, 0
		return
	}
	p.relax()
	avg := p.area / dt
	p.area, p.areaT = 0, 0
	p.fill += (avg - p.fill) * (1 - math.Exp(-dt/fillSmoothing))
	errSec := (p.fill - float64(p.target)) / float64(p.srcRate)
	p.integ = min(max(p.integ+errSec*dt, -maxAdj/ki), maxAdj/ki)
	p.adj = min(max(kp*errSec+ki*p.integ, -maxAdj), maxAdj)
	p.step = float64(p.srcRate) / p.outRate * (1 + p.adj)
}

// State reports the current playback state.
func (p *Playout) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func (p *Playout) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Stats{
		State:      p.state,
		SampleRate: p.srcRate,
		Target:     p.targetTime,
		Requested:  p.baseTarget,
		DriftPPM:   p.adj * 1e6,
		Underruns:  p.underruns,
		Skips:      p.skips,
	}
	if p.srcRate > 0 {
		level := float64(p.ring.n)
		if p.state == Playing {
			level = p.fill
		}
		s.Buffered = time.Duration(level / float64(p.srcRate) * float64(time.Second))
	}
	return s
}

// ring is a FIFO of stereo frames that overwrites the oldest frame when full.
type ring struct {
	buf  []float32 // interleaved, 2 values per frame
	head int       // index of the oldest frame
	n    int       // frames stored
}

func (r *ring) reset(capFrames int) {
	r.buf = make([]float32, 2*capFrames)
	r.head, r.n = 0, 0
}

func (r *ring) clear() { r.head, r.n = 0, 0 }

func (r *ring) cap() int { return len(r.buf) / 2 }

func (r *ring) push(left, right float32) {
	c := r.cap()
	if c == 0 {
		return
	}
	if r.n == c {
		r.discard(1)
	}
	i := (r.head + r.n) % c
	r.buf[2*i], r.buf[2*i+1] = left, right
	r.n++
}

func (r *ring) pop() (float32, float32, bool) {
	if r.n == 0 {
		return 0, 0, false
	}
	i := r.head
	r.head = (r.head + 1) % r.cap()
	r.n--
	return r.buf[2*i], r.buf[2*i+1], true
}

func (r *ring) discard(n int) {
	n = min(n, r.n)
	if n <= 0 {
		return
	}
	r.head = (r.head + n) % r.cap()
	r.n -= n
}
