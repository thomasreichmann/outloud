// Package coreaudio captures what a Mac plays, through a Core Audio process
// tap (macOS 14.2 or later).
//
// A tap mixes the output of chosen processes into an input stream. Reading it
// takes a private aggregate device that contains the tap, clocked by the
// default output device, with an IOProc on that aggregate.
package coreaudio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"github.com/thomasreichmann/outloud/internal/source"
)

const (
	systemObject = 1
	elementMain  = 0

	scopeGlobal             = 0x676c6f62 // 'glob'
	propTranslatePIDToProc  = 0x69643270 // 'id2p'
	propDefaultOutputDevice = 0x644f7574 // 'dOut'
	propDeviceUID           = 0x75696420 // 'uid '
	propName                = 0x6c6e616d // 'lnam'
	propTapFormat           = 0x74666d74 // 'tfmt'

	formatLinearPCM     = 0x6c70636d // 'lpcm'
	formatFlagFloat     = 1 << 0
	formatFlagNonInterl = 1 << 5

	// tccGranted is what TCCAccessPreflight returns once the user allowed it.
	tccGranted = 0
	tccDenied  = 1
)

const (
	// The tap keeps delivering digital silence while nothing plays; this
	// much of it means playback stopped.
	silenceIdle = 500 * time.Millisecond
	// A gap this long without any buffer also means stopped.
	gapIdle    = 100 * time.Millisecond
	checkEvery = time.Second
	// Once nothing has played this long, capture stops so the Mac can sleep.
	// Shorter would restart it between tracks and notification sounds.
	quietStop   = 10 * time.Second
	retryDelay  = 2 * time.Second
	chunkQueue  = 64
	tapName     = "Outloud"
	captureHint = "allow Outloud in System Settings > Privacy & Security > Screen & System Audio Recording"
)

var (
	errOutputChanged = errors.New("default output device changed")
	errQuiet         = errors.New("nothing plays")
)

type propertyAddress struct {
	selector, scope, element uint32
}

type streamDescription struct {
	sampleRate       float64
	formatID         uint32
	formatFlags      uint32
	bytesPerPacket   uint32
	framesPerPacket  uint32
	bytesPerFrame    uint32
	channelsPerFrame uint32
	bitsPerChannel   uint32
	reserved         uint32
}

// audioBuffer is an AudioBuffer; an AudioBufferList is a uint32 count,
// padded to 8 bytes, followed by that many of these.
type audioBuffer struct {
	channels uint32
	size     uint32
	data     unsafe.Pointer
}

var (
	loadOnce sync.Once
	loadErr  error

	audioObjectGetPropertyData          func(obj uint32, addr *propertyAddress, qualSize uint32, qual unsafe.Pointer, size *uint32, data unsafe.Pointer) int32
	audioHardwareCreateProcessTap       func(desc objc.ID, tap *uint32) int32
	audioHardwareDestroyProcessTap      func(tap uint32) int32
	audioHardwareCreateAggregateDevice  func(desc objc.ID, dev *uint32) int32
	audioHardwareDestroyAggregateDevice func(dev uint32) int32
	audioDeviceCreateIOProcID           func(dev uint32, proc uintptr, client unsafe.Pointer, id *uintptr) int32
	audioDeviceDestroyIOProcID          func(dev uint32, id uintptr) int32
	audioDeviceStart                    func(dev uint32, id uintptr) int32
	audioDeviceStop                     func(dev uint32, id uintptr) int32
	// TCCAccessPreflight is private API, used only to explain silence and
	// to have macOS ask early.
	tccAccessPreflight func(service objc.ID, options uintptr) int32
	// answered is set once a tap has opened, which waits for the answer to
	// macOS's question. TCCAccessPreflight keeps reporting what it found the
	// first time, so after that its "not decided" is stale.
	answered atomic.Bool

	ioProcPtr uintptr
	// active is the session the IOProc delivers to. One callback serves every
	// session, since callbacks are never freed.
	active atomic.Pointer[session]

	sel = map[string]objc.SEL{}
)

func load() error {
	loadOnce.Do(func() {
		ca, err := purego.Dlopen("/System/Library/Frameworks/CoreAudio.framework/CoreAudio", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			loadErr = err
			return
		}
		if _, err := purego.Dlopen("/System/Library/Frameworks/Foundation.framework/Foundation", purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
			loadErr = err
			return
		}
		for name, fn := range map[string]any{
			"AudioObjectGetPropertyData":          &audioObjectGetPropertyData,
			"AudioObjectGetPropertyDataSize":      &audioObjectGetPropertyDataSize,
			"AudioObjectAddPropertyListener":      &audioObjectAddPropertyListener,
			"AudioObjectRemovePropertyListener":   &audioObjectRemovePropertyListener,
			"AudioHardwareCreateProcessTap":       &audioHardwareCreateProcessTap,
			"AudioHardwareDestroyProcessTap":      &audioHardwareDestroyProcessTap,
			"AudioHardwareCreateAggregateDevice":  &audioHardwareCreateAggregateDevice,
			"AudioHardwareDestroyAggregateDevice": &audioHardwareDestroyAggregateDevice,
			"AudioDeviceCreateIOProcID":           &audioDeviceCreateIOProcID,
			"AudioDeviceDestroyIOProcID":          &audioDeviceDestroyIOProcID,
			"AudioDeviceStart":                    &audioDeviceStart,
			"AudioDeviceStop":                     &audioDeviceStop,
		} {
			sym, err := purego.Dlsym(ca, name)
			if err != nil {
				loadErr = fmt.Errorf("this macOS has no %s; system audio capture needs macOS 14.2 or later", name)
				return
			}
			purego.RegisterFunc(fn, sym)
		}
		if objc.GetClass("CATapDescription") == 0 {
			loadErr = errors.New("system audio capture needs macOS 14.2 or later")
			return
		}
		if tcc, err := purego.Dlopen("/System/Library/PrivateFrameworks/TCC.framework/TCC", purego.RTLD_NOW|purego.RTLD_GLOBAL); err == nil {
			if sym, err := purego.Dlsym(tcc, "TCCAccessPreflight"); err == nil {
				purego.RegisterFunc(&tccAccessPreflight, sym)
			}
		}
		for _, name := range []string{
			"alloc", "init", "new", "release", "drain", "UUID", "UUIDString", "UTF8String",
			"stringWithUTF8String:", "numberWithUnsignedInt:", "numberWithBool:",
			"arrayWithObject:", "array", "dictionary", "setObject:forKey:",
			"initStereoGlobalTapButExcludeProcesses:", "setName:", "setPrivate:",
		} {
			sel[name] = objc.RegisterName(name)
		}
		ioProcPtr = purego.NewCallback(ioProc)
		listenerPtr = purego.NewCallback(listener)
	})
	return loadErr
}

// Available reports why this Mac cannot capture its sound, or nil if it can.
func Available() error { return load() }

// Permission reports whether macOS lets this process record system audio:
// "granted", "denied", "not decided", or "unknown" once it was answered in a
// way this process cannot learn.
func Permission() string {
	if load() != nil || tccAccessPreflight == nil {
		return "unknown"
	}
	switch tccAccessPreflight(nsString("kTCCServiceAudioCapture"), 0) {
	case tccGranted:
		return "granted"
	case tccDenied:
		return "denied"
	}
	if answered.Load() {
		return "unknown"
	}
	return "not decided"
}

// Tap captures everything this Mac plays except the sound of this process
// itself, so a receiver never sends back what it is playing. It captures
// only while another process plays sound, since a running capture keeps the
// Mac from sleeping. It implements source.Source.
type Tap struct{}

func (t *Tap) Run(ctx context.Context, emit func(source.Event)) error {
	if err := load(); err != nil {
		return err
	}
	// Without the watch, capture runs all the time.
	w, _ := watchPlayers()
	defer w.close()
	for {
		if !ready(ctx, w, emit) {
			return nil
		}
		err := t.capture(ctx, emit, w)
		if ctx.Err() != nil {
			return nil
		}
		emit(source.Event{Kind: source.Idle})
		delay := retryDelay
		if errors.Is(err, errOutputChanged) || errors.Is(err, errQuiet) {
			delay = 0
		} else {
			emit(source.Event{Kind: source.Status, Err: err})
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// ready waits until another process plays sound, and reports false if ctx
// is done first. While macOS has yet to ask for permission it returns at
// once, so that macOS asks while someone is setting Outloud up.
func ready(ctx context.Context, w *players, emit func(source.Event)) bool {
	switch Permission() {
	case "not decided":
		return true
	case "denied":
		// Say so now rather than when something plays. Capture still starts
		// then: this process would not learn that permission was given.
		emit(source.Event{Kind: source.Status, Err: fmt.Errorf("%w: %s", source.ErrDenied, captureHint)})
	}
	return w.wait(ctx)
}

// capture runs one tap until it fails, the default output device changes,
// nothing has played for quietStop, or ctx is done.
func (t *Tap) capture(ctx context.Context, emit func(source.Event), w *players) error {
	// Until someone answers macOS's prompt, opening the tap waits.
	if Permission() == "not decided" {
		emit(source.Event{Kind: source.Status, Err: fmt.Errorf("%w: macOS asks on this Mac's screen", source.ErrNeedsPermission)})
	}
	s, err := open()
	if err != nil {
		return err
	}
	defer s.close()

	conv := source.NewConverter(source.Float32, s.channels, 0)
	emit(source.Event{Kind: source.Format, Rate: s.rate, Device: s.device})
	if Permission() == "denied" {
		emit(source.Event{Kind: source.Status, Err: fmt.Errorf("%w: %s", source.ErrDenied, captureHint)})
	}

	tick := time.NewTicker(gapIdle / 2)
	defer tick.Stop()
	var (
		active     bool
		lastPacket time.Time
		lastSound  time.Time
		lastCheck  = time.Now()
		quietSince time.Time // since when no other process plays
	)
	checkPlayers := func(now time.Time) {
		if playing, err := w.playing(); err != nil || playing {
			quietSince = time.Time{}
		} else if quietSince.IsZero() {
			quietSince = now
		}
	}
	checkPlayers(time.Now())
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.changes():
			checkPlayers(time.Now())
		case b := <-s.chunks:
			now := time.Now()
			samples, silent := conv.Convert(nil, b, len(b)/conv.FrameBytes())
			lastPacket = now
			if !silent {
				lastSound = now
			}
			if now.Sub(lastSound) < silenceIdle {
				emit(source.Event{Kind: source.Audio, Samples: samples})
				active = true
			} else if active {
				emit(source.Event{Kind: source.Idle})
				active = false
			}
		case now := <-tick.C:
			if active && now.Sub(lastPacket) > gapIdle {
				emit(source.Event{Kind: source.Idle})
				active = false
			}
			if !quietSince.IsZero() && now.Sub(quietSince) >= quietStop {
				return errQuiet
			}
			if now.Sub(lastCheck) >= checkEvery {
				lastCheck = now
				if dev, err := defaultOutput(); err == nil && dev != s.output {
					return errOutputChanged
				}
			}
		}
	}
}

// session is one running tap and the aggregate device that reads it.
type session struct {
	tap, aggregate uint32
	procID         uintptr
	desc           objc.ID
	output         uint32 // the default output device that clocks the aggregate
	device         string // its name
	rate           int
	channels       int
	nonInterleaved bool
	chunks         chan []byte
	started        bool
}

func open() (s *session, err error) {
	// Autorelease pools are per thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pool := objc.ID(objc.GetClass("NSAutoreleasePool")).Send(sel["new"])
	defer pool.Send(sel["drain"])

	s = &session{chunks: make(chan []byte, chunkQueue)}
	defer func() {
		if err != nil {
			s.close()
		}
	}()

	// Exclude this process, which plays what a PC sends, so that sound never
	// travels back. A process that has not touched Core Audio yet has no
	// object; then there is nothing to exclude.
	pid := int32(os.Getpid())
	var self uint32
	_ = getProperty(systemObject, propTranslatePIDToProc, unsafe.Pointer(&pid), 4, unsafe.Pointer(&self), 4)
	exclude := objc.ID(objc.GetClass("NSArray")).Send(sel["array"])
	if self != 0 {
		num := objc.ID(objc.GetClass("NSNumber")).Send(sel["numberWithUnsignedInt:"], self)
		exclude = objc.ID(objc.GetClass("NSArray")).Send(sel["arrayWithObject:"], num)
	}
	s.desc = objc.ID(objc.GetClass("CATapDescription")).Send(sel["alloc"]).
		Send(sel["initStereoGlobalTapButExcludeProcesses:"], exclude)
	if s.desc == 0 {
		return nil, errors.New("creating the tap description failed")
	}
	s.desc.Send(sel["setName:"], nsString(tapName))
	s.desc.Send(sel["setPrivate:"], true)
	if st := audioHardwareCreateProcessTap(s.desc, &s.tap); st != 0 {
		return nil, fmt.Errorf("creating the system audio tap: %v", osStatus(st))
	}
	tapUID := s.desc.Send(sel["UUID"]).Send(sel["UUIDString"])

	var f streamDescription
	if err := getProperty(s.tap, propTapFormat, nil, 0, unsafe.Pointer(&f), uint32(unsafe.Sizeof(f))); err != nil {
		return nil, fmt.Errorf("reading the tap's format: %w", err)
	}
	if f.formatID != formatLinearPCM || f.formatFlags&formatFlagFloat == 0 || f.bitsPerChannel != 32 || f.channelsPerFrame == 0 {
		return nil, fmt.Errorf("unsupported tap format (%#x, flags %#x, %d bits)", f.formatID, f.formatFlags, f.bitsPerChannel)
	}
	s.rate, s.channels = int(f.sampleRate), int(f.channelsPerFrame)
	s.nonInterleaved = f.formatFlags&formatFlagNonInterl != 0

	if s.output, err = defaultOutput(); err != nil {
		return nil, err
	}
	var outUID objc.ID // a CFStringRef we own
	if err := getProperty(s.output, propDeviceUID, nil, 0, unsafe.Pointer(&outUID), 8); err != nil {
		return nil, fmt.Errorf("reading the output device's ID: %w", err)
	}
	defer outUID.Send(sel["release"])
	var name objc.ID
	if getProperty(s.output, propName, nil, 0, unsafe.Pointer(&name), 8) == nil && name != 0 {
		s.device = goString(name)
		name.Send(sel["release"])
	}

	sub := dict("uid", outUID)
	tap := dict("uid", tapUID, "drift", nsBool(true))
	agg := dict(
		"name", nsString(tapName),
		"uid", objc.ID(objc.GetClass("NSUUID")).Send(sel["UUID"]).Send(sel["UUIDString"]),
		"master", outUID,
		"private", nsBool(true),
		"stacked", nsBool(false),
		"tapautostart", nsBool(true),
		"subdevices", array(sub),
		"taps", array(tap),
	)
	if st := audioHardwareCreateAggregateDevice(agg, &s.aggregate); st != 0 {
		return nil, fmt.Errorf("creating the capture device: %v", osStatus(st))
	}
	if st := audioDeviceCreateIOProcID(s.aggregate, ioProcPtr, nil, &s.procID); st != 0 {
		return nil, fmt.Errorf("creating the capture callback: %v", osStatus(st))
	}
	active.Store(s)
	if st := audioDeviceStart(s.aggregate, s.procID); st != 0 {
		return nil, fmt.Errorf("starting capture: %v", osStatus(st))
	}
	s.started = true
	answered.Store(true)
	return s, nil
}

func (s *session) close() {
	if s.started {
		audioDeviceStop(s.aggregate, s.procID)
	}
	active.CompareAndSwap(s, nil)
	if s.procID != 0 {
		audioDeviceDestroyIOProcID(s.aggregate, s.procID)
	}
	if s.aggregate != 0 {
		audioHardwareDestroyAggregateDevice(s.aggregate)
	}
	if s.tap != 0 {
		audioHardwareDestroyProcessTap(s.tap)
	}
	if s.desc != 0 {
		s.desc.Send(sel["release"])
	}
	*s = session{chunks: s.chunks}
}

// ioProc runs on Core Audio's real-time thread. It copies the tap's frames,
// interleaved, and hands them to the session without blocking.
func ioProc(_ uint32, _, input, _, _, _, _ unsafe.Pointer) int32 {
	s := active.Load()
	if s == nil || input == nil {
		return 0
	}
	n := *(*uint32)(input)
	if n == 0 {
		return 0
	}
	bufs := unsafe.Slice((*audioBuffer)(unsafe.Add(input, 8)), n)
	var b []byte
	if !s.nonInterleaved || n == 1 {
		buf := bufs[0]
		if buf.data == nil || int(buf.channels) != s.channels {
			return 0
		}
		b = make([]byte, buf.size)
		copy(b, unsafe.Slice((*byte)(buf.data), buf.size))
	} else {
		// One buffer per channel.
		if int(n) < s.channels {
			return 0
		}
		frames := int(bufs[0].size) / 4
		b = make([]byte, frames*4*s.channels)
		for ch := range s.channels {
			if bufs[ch].data == nil || int(bufs[ch].size) < frames*4 {
				return 0
			}
			src := unsafe.Slice((*[4]byte)(bufs[ch].data), frames)
			for f := range frames {
				copy(b[(f*s.channels+ch)*4:], src[f][:])
			}
		}
	}
	select {
	case s.chunks <- b:
	default: // the reader is stalled; drop rather than block the audio thread
	}
	return 0
}

func defaultOutput() (uint32, error) {
	var dev uint32
	if err := getProperty(systemObject, propDefaultOutputDevice, nil, 0, unsafe.Pointer(&dev), 4); err != nil {
		return 0, fmt.Errorf("finding the default output device: %w", err)
	}
	if dev == 0 {
		return 0, errors.New("this Mac has no output device")
	}
	return dev, nil
}

func getProperty(obj, selector uint32, qual unsafe.Pointer, qualSize uint32, out unsafe.Pointer, size uint32) error {
	addr := propertyAddress{selector: selector, scope: scopeGlobal, element: elementMain}
	if st := audioObjectGetPropertyData(obj, &addr, qualSize, qual, &size, out); st != 0 {
		return osStatus(st)
	}
	return nil
}

// osStatus formats a Core Audio error, which is usually four characters.
type osStatus int32

func (e osStatus) Error() string {
	u := uint32(e)
	b := []byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return fmt.Sprintf("error %d", int32(e))
		}
	}
	return fmt.Sprintf("error '%s'", b)
}

func nsString(s string) objc.ID {
	return objc.ID(objc.GetClass("NSString")).Send(sel["stringWithUTF8String:"], s)
}

func nsBool(v bool) objc.ID {
	return objc.ID(objc.GetClass("NSNumber")).Send(sel["numberWithBool:"], v)
}

func array(v objc.ID) objc.ID {
	return objc.ID(objc.GetClass("NSArray")).Send(sel["arrayWithObject:"], v)
}

// dict builds an NSDictionary from string keys and object values.
func dict(kv ...any) objc.ID {
	d := objc.ID(objc.GetClass("NSMutableDictionary")).Send(sel["dictionary"])
	for i := 0; i < len(kv); i += 2 {
		d.Send(sel["setObject:forKey:"], kv[i+1].(objc.ID), nsString(kv[i].(string)))
	}
	return d
}

func goString(s objc.ID) string {
	p := objc.Send[*byte](s, sel["UTF8String"])
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}
