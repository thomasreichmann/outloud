package wasapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/thomasreichmann/outloud/internal/source"
)

// Device is a playback device that can be captured.
type Device struct {
	ID      string
	Name    string
	Default bool
}

// Devices lists the active playback devices.
func Devices() ([]Device, error) {
	var out []Device
	err := withCOM(func(e *enumerator) error {
		var defID string
		if d, err := e.defaultRender(); err == nil {
			defID, _ = d.id()
			d.Release()
		}
		devs, err := e.renderDevices()
		if err != nil {
			return err
		}
		for _, d := range devs {
			id, _ := d.id()
			name, _ := d.name()
			d.Release()
			out = append(out, Device{ID: id, Name: name, Default: id == defID})
		}
		return nil
	})
	return out, err
}

// Loopback captures everything a playback device plays. It implements
// source.Source.
type Loopback struct {
	// Device selects a device by exact ID or by case-insensitive substring
	// of its name. Empty follows the Windows default playback device.
	Device string
	// NativeFormat captures in the device's mix format and converts in Go,
	// instead of asking Windows for stereo float. For diagnostics.
	NativeFormat bool
}

const (
	bufferDuration = 200 * time.Millisecond
	pollInterval   = 5 * time.Millisecond
	// Windows delivers no packets at all while nothing is playing; a gap
	// this long means playback stopped.
	gapIdle = 100 * time.Millisecond
	// Some apps keep a stream open playing digital silence; this much of it
	// also counts as idle, so the receiver can rest.
	silenceIdle    = 2 * time.Second
	volumeInterval = 200 * time.Millisecond
	defaultCheck   = time.Second
	retryDelay     = time.Second
)

var errDefaultChanged = errors.New("default playback device changed")

func (l *Loopback) Run(ctx context.Context, emit func(source.Event)) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := initCOM(); err != nil {
		return err
	}
	defer windows.CoUninitialize()
	e, err := newEnumerator()
	if err != nil {
		return err
	}
	defer e.Release()

	for {
		err := l.capture(ctx, e, emit)
		if ctx.Err() != nil {
			return nil
		}
		emit(source.Event{Kind: source.Idle})
		delay := retryDelay
		if errors.Is(err, errDefaultChanged) {
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

// capture runs one capture session on one device until it fails, the
// default device changes, or ctx is done.
func (l *Loopback) capture(ctx context.Context, e *enumerator, emit func(source.Event)) error {
	dev, err := l.open(e)
	if err != nil {
		return err
	}
	defer dev.Release()
	id, _ := dev.id()
	name, _ := dev.name()

	client, conv, rate, err := l.initialize(dev)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	defer client.Release()
	cc, err := client.captureClient()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	defer cc.Release()
	vol, err := dev.endpointVolume()
	if err == nil {
		defer vol.Release()
	}
	if err := client.start(); err != nil {
		return fmt.Errorf("%s: starting capture: %w", name, err)
	}
	defer client.stop()

	emit(source.Event{Kind: source.Format, Rate: rate, Device: name})
	lastGain := float32(-1)
	sendVolume := func() {
		if vol == nil {
			return
		}
		if g, err := gain(vol); err == nil && math.Abs(float64(g-lastGain)) > 1e-4 {
			emit(source.Event{Kind: source.Volume, Gain: g})
			lastGain = g
		}
	}
	sendVolume()

	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	var (
		active     bool
		lastPacket time.Time
		lastSound  time.Time
		lastVolume = time.Now()
		lastCheck  = time.Now()
	)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
		}
		now := time.Now()

		var samples []int16
		got, silent := false, true
		for {
			n, err := cc.nextPacketSize()
			if err != nil {
				return err
			}
			if n == 0 {
				break
			}
			data, frames, flags, err := cc.getBuffer()
			if err != nil {
				return err
			}
			var src []byte
			if flags&bufferflagsSilent == 0 && frames > 0 {
				src = unsafe.Slice(data, int(frames)*conv.FrameBytes())
			}
			var s bool
			samples, s = conv.Convert(samples, src, int(frames))
			silent = silent && s
			got = got || frames > 0
			if err := cc.releaseBuffer(frames); err != nil {
				return err
			}
		}

		switch {
		case got:
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
		case active && now.Sub(lastPacket) > gapIdle:
			emit(source.Event{Kind: source.Idle})
			active = false
		}

		if now.Sub(lastVolume) >= volumeInterval {
			lastVolume = now
			sendVolume()
		}
		if l.Device == "" && now.Sub(lastCheck) >= defaultCheck {
			lastCheck = now
			if d, err := e.defaultRender(); err == nil {
				cur, _ := d.id()
				d.Release()
				if cur != id {
					return errDefaultChanged
				}
			}
		}
	}
}

func (l *Loopback) open(e *enumerator) (*device, error) {
	if l.Device == "" {
		d, err := e.defaultRender()
		if errors.Is(err, errNotFound) {
			return nil, errors.New("Windows has no playback device")
		}
		return d, err
	}
	devs, err := e.renderDevices()
	if err != nil {
		return nil, err
	}
	var match *device
	want := strings.ToLower(l.Device)
	for _, d := range devs {
		if match == nil {
			id, _ := d.id()
			name, _ := d.name()
			if id == l.Device || strings.Contains(strings.ToLower(name), want) {
				match = d
				continue
			}
		}
		d.Release()
	}
	if match == nil {
		return nil, fmt.Errorf("no active playback device matches %q", l.Device)
	}
	return match, nil
}

// initialize sets up a loopback stream on dev. It asks Windows for stereo
// float at the mix rate, so that Windows does any downmixing, and falls back
// to the mix format itself if that is refused.
func (l *Loopback) initialize(dev *device) (*audioClient, *source.Converter, int, error) {
	client, err := dev.audioClient()
	if err != nil {
		return nil, nil, 0, err
	}
	mix, err := client.mixFormat()
	if err != nil {
		client.Release()
		return nil, nil, 0, err
	}
	rate := int(mix.SamplesPerSec)
	hns := int64(bufferDuration / 100)

	if !l.NativeFormat {
		want := waveFormat{
			FormatTag:      waveFormatExtensible,
			Channels:       2,
			SamplesPerSec:  mix.SamplesPerSec,
			AvgBytesPerSec: mix.SamplesPerSec * 8,
			BlockAlign:     8,
			BitsPerSample:  32,
			CbSize:         22,
			ValidBits:      32,
			ChannelMask:    0x3,
			SubFormat:      subtypeIEEEFloat,
		}
		flags := uint32(streamflagsLoopback | streamflagsAutoConvertPCM | streamflagsSrcDefaultQuality)
		if err := client.initialize(flags, hns, &want); err == nil {
			return client, source.NewConverter(source.Float32, 2, 0x3), rate, nil
		}
		// A client whose Initialize failed is not reused.
		client.Release()
		if client, err = dev.audioClient(); err != nil {
			return nil, nil, 0, err
		}
	}

	kind, err := sampleKind(mix)
	if err != nil {
		client.Release()
		return nil, nil, 0, err
	}
	if err := client.initialize(streamflagsLoopback, hns, &mix); err != nil {
		client.Release()
		return nil, nil, 0, fmt.Errorf("starting loopback capture: %w", err)
	}
	return client, source.NewConverter(kind, int(mix.Channels), mix.ChannelMask), rate, nil
}

func sampleKind(wf waveFormat) (source.SampleKind, error) {
	ext := wf.FormatTag == waveFormatExtensible
	isFloat := wf.FormatTag == waveFormatIEEEFloat || (ext && wf.SubFormat == subtypeIEEEFloat)
	isPCM := wf.FormatTag == waveFormatPCM || (ext && wf.SubFormat == subtypePCM)
	bits := 0
	if wf.Channels > 0 {
		bits = int(wf.BlockAlign) / int(wf.Channels) * 8
	}
	switch {
	case isFloat && bits == 32:
		return source.Float32, nil
	case isPCM && bits == 16:
		return source.Int16, nil
	case isPCM && bits == 24:
		return source.Int24, nil
	case isPCM && bits == 32:
		return source.Int32, nil
	}
	return 0, fmt.Errorf("unsupported mix format (tag 0x%X, %d-bit)", wf.FormatTag, bits)
}

// gain is the device's volume as a linear factor: what Windows would apply
// to its own speakers.
func gain(v *endpointVolume) (float32, error) {
	if m, err := v.muted(); err != nil || m {
		return 0, err
	}
	db, err := v.levelDB()
	if err != nil {
		return 0, err
	}
	return float32(math.Pow(10, float64(db)/20)), nil
}
