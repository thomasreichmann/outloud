package coreaudio

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	unitTypeOutput     = 0x61756f75 // 'auou'
	unitDefaultOutput  = 0x64656620 // 'def '
	unitManufacturer   = 0x6170706c // 'appl'
	unitScopeGlobal    = 0
	unitScopeInput     = 1
	propStreamFormat   = 8
	propRenderCallback = 23
	propCurrentDevice  = 2000
	formatFlagPacked   = 1 << 3

	propBufferFrameSize = 0x6673697a // 'fsiz'
	propLatency         = 0x6c746e63 // 'ltnc'
	propSafetyOffset    = 0x73616674 // 'saft'
	propStreams         = 0x73746d23 // 'stm#'
	propOverload        = 0x6f766c72 // 'ovrl'
)

type componentDescription struct {
	componentType, subType, manufacturer, flags, flagsMask uint32
}

type renderCallback struct {
	proc   uintptr
	refCon unsafe.Pointer
}

var (
	playerOnce sync.Once
	playerErr  error

	audioComponentFindNext         func(after uintptr, desc *componentDescription) uintptr
	audioComponentInstanceNew      func(comp uintptr, unit *uintptr) int32
	audioComponentInstanceDispose  func(unit uintptr) int32
	audioUnitSetProperty           func(unit uintptr, prop, scope, element uint32, data unsafe.Pointer, size uint32) int32
	audioUnitGetProperty           func(unit uintptr, prop, scope, element uint32, data unsafe.Pointer, size *uint32) int32
	audioUnitInitialize            func(unit uintptr) int32
	audioUnitUninitialize          func(unit uintptr) int32
	audioOutputUnitStart           func(unit uintptr) int32
	audioOutputUnitStop            func(unit uintptr) int32
	renderPtr, overloadListenerPtr uintptr

	// playing is the Player the render callback reads for. Like the IOProc,
	// one callback serves every Player.
	playing atomic.Pointer[Player]
)

func loadPlayer() error {
	playerOnce.Do(func() {
		if playerErr = loadBase(); playerErr != nil {
			return
		}
		at, err := purego.Dlopen("/System/Library/Frameworks/AudioToolbox.framework/AudioToolbox", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			playerErr = err
			return
		}
		for name, fn := range map[string]any{
			"AudioComponentFindNext":        &audioComponentFindNext,
			"AudioComponentInstanceNew":     &audioComponentInstanceNew,
			"AudioComponentInstanceDispose": &audioComponentInstanceDispose,
			"AudioUnitSetProperty":          &audioUnitSetProperty,
			"AudioUnitGetProperty":          &audioUnitGetProperty,
			"AudioUnitInitialize":           &audioUnitInitialize,
			"AudioUnitUninitialize":         &audioUnitUninitialize,
			"AudioOutputUnitStart":          &audioOutputUnitStart,
			"AudioOutputUnitStop":           &audioOutputUnitStop,
		} {
			sym, err := purego.Dlsym(at, name)
			if err != nil {
				playerErr = fmt.Errorf("AudioToolbox has no %s", name)
				return
			}
			purego.RegisterFunc(fn, sym)
		}
		renderPtr = purego.NewCallback(render)
		overloadListenerPtr = purego.NewCallback(overloadListener)
	})
	return playerErr
}

// Player plays a stream through the default output device, following it
// when it changes, with as little buffering as the device allows: each time
// the device needs audio, the player reads it from the source right then.
// It plays 32-bit float stereo, interleaved, at the rate it was made with.
// Only one Player plays at a time.
type Player struct {
	unit uintptr
	src  io.Reader

	mu      sync.Mutex
	running bool
	device  uint32 // the device watched for overloads

	overloads atomic.Int64
}

// NewPlayer starts playing src, which must fill whatever it is asked to
// read, at rate Hz. frames is how much the device should ask for each time,
// or 0 for its default.
func NewPlayer(src io.Reader, rate, frames int) (p *Player, err error) {
	if err := loadPlayer(); err != nil {
		return nil, err
	}
	p = &Player{src: src}
	desc := componentDescription{componentType: unitTypeOutput, subType: unitDefaultOutput, manufacturer: unitManufacturer}
	comp := audioComponentFindNext(0, &desc)
	if comp == 0 {
		return nil, errors.New("this Mac has no default output unit")
	}
	if st := audioComponentInstanceNew(comp, &p.unit); st != 0 {
		return nil, fmt.Errorf("opening the default output: %v", osStatus(st))
	}
	defer func() {
		if err != nil {
			audioComponentInstanceDispose(p.unit)
		}
	}()
	f := streamDescription{
		sampleRate:       float64(rate),
		formatID:         formatLinearPCM,
		formatFlags:      formatFlagFloat | formatFlagPacked,
		bytesPerPacket:   8,
		framesPerPacket:  1,
		bytesPerFrame:    8,
		channelsPerFrame: 2,
		bitsPerChannel:   32,
	}
	if st := audioUnitSetProperty(p.unit, propStreamFormat, unitScopeInput, 0, unsafe.Pointer(&f), uint32(unsafe.Sizeof(f))); st != 0 {
		return nil, fmt.Errorf("setting the output format: %v", osStatus(st))
	}
	cb := renderCallback{proc: renderPtr}
	if st := audioUnitSetProperty(p.unit, propRenderCallback, unitScopeInput, 0, unsafe.Pointer(&cb), uint32(unsafe.Sizeof(cb))); st != 0 {
		return nil, fmt.Errorf("setting the output callback: %v", osStatus(st))
	}
	if frames > 0 {
		n := uint32(frames)
		// Devices round this to what they can do, or refuse, and then use
		// their own.
		audioUnitSetProperty(p.unit, propBufferFrameSize, unitScopeGlobal, 0, unsafe.Pointer(&n), 4)
	}
	if st := audioUnitInitialize(p.unit); st != 0 {
		return nil, fmt.Errorf("preparing the output: %v", osStatus(st))
	}
	playing.Store(p)
	if st := audioOutputUnitStart(p.unit); st != 0 {
		playing.CompareAndSwap(p, nil)
		return nil, fmt.Errorf("starting the output: %v", osStatus(st))
	}
	p.running = true
	p.watchOverloads()
	return p, nil
}

// Wake starts the device, if it is not running.
func (p *Player) Wake() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return
	}
	if audioOutputUnitStart(p.unit) == 0 {
		p.running = true
		p.watchOverloads()
	}
}

// Sleep stops the device, so that it no longer keeps the Mac awake.
func (p *Player) Sleep() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		audioOutputUnitStop(p.unit)
		p.running = false
	}
}

// Overloads counts the times the device had to go without audio, because
// the player was too late.
func (p *Player) Overloads() int64 { return p.overloads.Load() }

// Latency is how long audio takes from being read to being heard: the
// device's buffer, its latency and safety offset, and its stream's latency.
func (p *Player) Latency() (time.Duration, error) {
	dev := p.currentDevice()
	if dev == 0 {
		return 0, errors.New("no output device")
	}
	var rate float64
	addr := propertyAddress{0x6e737274 /* 'nsrt' */, scopeGlobal, elementMain}
	size := uint32(8)
	if st := audioObjectGetPropertyData(dev, &addr, 0, nil, &size, unsafe.Pointer(&rate)); st != 0 || rate == 0 {
		return 0, fmt.Errorf("reading the output's rate: %v", osStatus(st))
	}
	var frames uint32
	for _, a := range []propertyAddress{
		{propBufferFrameSize, scopeGlobal, elementMain},
		{propLatency, scopeOutput, elementMain},
		{propSafetyOffset, scopeOutput, elementMain},
	} {
		var v uint32
		size := uint32(4)
		if audioObjectGetPropertyData(dev, &a, 0, nil, &size, unsafe.Pointer(&v)) == 0 {
			frames += v
		}
	}
	// The first output stream's latency, which for built-in speakers
	// includes their processing.
	addr = propertyAddress{propStreams, scopeOutput, elementMain}
	var streams [8]uint32
	size = uint32(len(streams) * 4)
	if audioObjectGetPropertyData(dev, &addr, 0, nil, &size, unsafe.Pointer(&streams[0])) == 0 && size >= 4 {
		var v uint32
		a := propertyAddress{propLatency, scopeGlobal, elementMain}
		size := uint32(4)
		if audioObjectGetPropertyData(streams[0], &a, 0, nil, &size, unsafe.Pointer(&v)) == 0 {
			frames += v
		}
	}
	return time.Duration(float64(frames) / rate * float64(time.Second)), nil
}

func (p *Player) currentDevice() uint32 {
	var dev uint32
	size := uint32(4)
	if audioUnitGetProperty(p.unit, propCurrentDevice, unitScopeGlobal, 0, unsafe.Pointer(&dev), &size) != 0 {
		return 0
	}
	return dev
}

// watchOverloads listens for overloads on the device in use, which may have
// changed since the player last started. p.mu is held.
func (p *Player) watchOverloads() {
	dev := p.currentDevice()
	if dev == p.device {
		return
	}
	addr := propertyAddress{propOverload, scopeGlobal, elementMain}
	if p.device != 0 {
		audioObjectRemovePropertyListener(p.device, &addr, overloadListenerPtr, nil)
	}
	p.device = 0
	if dev != 0 && audioObjectAddPropertyListener(dev, &addr, overloadListenerPtr, nil) == 0 {
		p.device = dev
	}
}

// render runs on the device's real-time thread: it fills the device's buffer
// from the source.
func render(_ unsafe.Pointer, _ *uint32, _ unsafe.Pointer, _, frames uint32, data unsafe.Pointer) int32 {
	if data == nil || *(*uint32)(data) == 0 {
		return 0
	}
	buf := (*audioBuffer)(unsafe.Add(data, 8))
	if buf.data == nil {
		return 0
	}
	b := unsafe.Slice((*byte)(buf.data), buf.size)
	b = b[:min(len(b), int(frames)*8)]
	p := playing.Load()
	if p == nil {
		clear(b)
		return 0
	}
	for n := 0; n < len(b); {
		m, err := p.src.Read(b[n:])
		n += m
		if err != nil || m == 0 {
			clear(b[n:])
			break
		}
	}
	return 0
}

func overloadListener(_, _ uint32, _, _ unsafe.Pointer) int32 {
	if p := playing.Load(); p != nil {
		p.overloads.Add(1)
	}
	return 0
}
