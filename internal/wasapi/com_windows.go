// Package wasapi captures what a Windows playback device is playing, using
// WASAPI loopback capture through hand-written COM bindings.
package wasapi

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procPropVariantClear = ole32.NewProc("PropVariantClear")
)

var (
	clsidMMDeviceEnumerator = windows.GUID{Data1: 0xBCDE0395, Data2: 0xE52F, Data3: 0x467C, Data4: [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIMMDeviceEnumerator  = windows.GUID{Data1: 0xA95664D2, Data2: 0x9614, Data3: 0x4F35, Data4: [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidIAudioClient         = windows.GUID{Data1: 0x1CB9AD4C, Data2: 0xDBFA, Data3: 0x4C32, Data4: [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidIAudioCaptureClient  = windows.GUID{Data1: 0xC8ADBD64, Data2: 0xE71E, Data3: 0x48A0, Data4: [8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}}
	iidIAudioEndpointVolume = windows.GUID{Data1: 0x5CDF2C82, Data2: 0x841E, Data3: 0x4546, Data4: [8]byte{0x97, 0x22, 0x0C, 0xF7, 0x40, 0x78, 0x22, 0x9A}}

	subtypePCM       = windows.GUID{Data1: 0x00000001, Data2: 0x0000, Data3: 0x0010, Data4: [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	subtypeIEEEFloat = windows.GUID{Data1: 0x00000003, Data2: 0x0000, Data3: 0x0010, Data4: [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}

	pkeyDeviceFriendlyName = propertyKey{
		fmtid: windows.GUID{Data1: 0xA45C254E, Data2: 0xDF1C, Data3: 0x4EFD, Data4: [8]byte{0x80, 0x20, 0x67, 0xD1, 0x46, 0xA8, 0x50, 0xE0}},
		pid:   14,
	}
)

const (
	clsctxAll = 0x17

	eRender   = 0
	eConsole  = 0
	stgmRead  = 0
	vtLPWSTR  = 31
	stateLive = 0x1 // DEVICE_STATE_ACTIVE

	audclntSharemodeShared = 0

	streamflagsLoopback          = 0x00020000
	streamflagsSrcDefaultQuality = 0x08000000
	streamflagsAutoConvertPCM    = 0x80000000

	bufferflagsSilent = 0x2

	waveFormatPCM        = 1
	waveFormatIEEEFloat  = 3
	waveFormatExtensible = 0xFFFE
)

// HRESULT is a failed COM call's status code.
type HRESULT uint32

const (
	errDeviceInvalidated   HRESULT = 0x88890004
	errUnsupportedFormat   HRESULT = 0x88890008
	errServiceNotRunning   HRESULT = 0x88890010
	errResourceInvalidated HRESULT = 0x88890026
	errNotFound            HRESULT = 0x80070490
)

func (h HRESULT) Error() string {
	switch h {
	case errDeviceInvalidated:
		return "audio device was removed or reconfigured"
	case errUnsupportedFormat:
		return "audio format not supported"
	case errServiceNotRunning:
		return "Windows Audio service is not running"
	case errResourceInvalidated:
		return "audio resources were invalidated"
	case errNotFound:
		return "audio device not found"
	}
	return fmt.Sprintf("COM error 0x%08X", uint32(h))
}

// comObj is the layout every COM interface pointer points at: a vtable.
// The array length is only an upper bound for indexing.
type comObj struct{ vtbl *[32]uintptr }

// call invokes vtable method i with the object as the implicit first
// argument.
//
//go:uintptrescapes
func (o *comObj) call(i int, args ...uintptr) error {
	hr, _, _ := syscall.SyscallN(o.vtbl[i], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	if int32(hr) < 0 {
		return HRESULT(hr)
	}
	return nil
}

func (o *comObj) Release() {
	syscall.SyscallN(o.vtbl[2], uintptr(unsafe.Pointer(o)))
}

type propertyKey struct {
	fmtid windows.GUID
	pid   uint32
}

type propVariant struct {
	vt  uint16
	_   [3]uint16
	ptr unsafe.Pointer
	_   uintptr
}

// waveFormat is WAVEFORMATEXTENSIBLE. The Go field offsets match the packed
// C layout (WAVEFORMATEX is 18 bytes, so Samples sits at 18).
type waveFormat struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	CbSize         uint16
	ValidBits      uint16
	ChannelMask    uint32
	SubFormat      windows.GUID
}

func initCOM() error {
	err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED)
	if err != nil && !errors.Is(err, syscall.Errno(1)) { // S_FALSE: already initialized
		return fmt.Errorf("CoInitializeEx: %w", err)
	}
	return nil
}

// withCOM runs fn on a fresh OS thread with COM initialized.
func withCOM(fn func(*enumerator) error) error {
	errc := make(chan error, 1)
	go func() {
		// Never unlocked: the thread ends with this goroutine.
		runtime.LockOSThread()
		if err := initCOM(); err != nil {
			errc <- err
			return
		}
		defer windows.CoUninitialize()
		e, err := newEnumerator()
		if err != nil {
			errc <- err
			return
		}
		defer e.Release()
		errc <- fn(e)
	}()
	return <-errc
}

// IMMDeviceEnumerator
type enumerator struct{ comObj }

func newEnumerator() (*enumerator, error) {
	var e *enumerator
	hr, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)),
		0,
		clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&e)))
	if int32(hr) < 0 {
		return nil, fmt.Errorf("creating device enumerator: %w", HRESULT(hr))
	}
	return e, nil
}

func (e *enumerator) renderDevices() ([]*device, error) {
	var c *collection
	if err := e.call(3, eRender, stateLive, uintptr(unsafe.Pointer(&c))); err != nil {
		return nil, err
	}
	defer c.Release()
	var n uint32
	if err := c.call(3, uintptr(unsafe.Pointer(&n))); err != nil {
		return nil, err
	}
	devs := make([]*device, 0, n)
	for i := range n {
		var d *device
		if err := c.call(4, uintptr(i), uintptr(unsafe.Pointer(&d))); err != nil {
			for _, d := range devs {
				d.Release()
			}
			return nil, err
		}
		devs = append(devs, d)
	}
	return devs, nil
}

func (e *enumerator) defaultRender() (*device, error) {
	var d *device
	if err := e.call(4, eRender, eConsole, uintptr(unsafe.Pointer(&d))); err != nil {
		return nil, err
	}
	return d, nil
}

// IMMDeviceCollection
type collection struct{ comObj }

// IMMDevice
type device struct{ comObj }

func (d *device) id() (string, error) {
	var p *uint16
	if err := d.call(5, uintptr(unsafe.Pointer(&p))); err != nil {
		return "", err
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(p))
	return windows.UTF16PtrToString(p), nil
}

func (d *device) name() (string, error) {
	var ps *comObj // IPropertyStore
	if err := d.call(4, stgmRead, uintptr(unsafe.Pointer(&ps))); err != nil {
		return "", err
	}
	defer ps.Release()
	var v propVariant
	if err := ps.call(5, uintptr(unsafe.Pointer(&pkeyDeviceFriendlyName)), uintptr(unsafe.Pointer(&v))); err != nil {
		return "", err
	}
	// A closure keeps v reachable until the deferred call runs.
	defer func() { procPropVariantClear.Call(uintptr(unsafe.Pointer(&v))) }()
	if v.vt != vtLPWSTR || v.ptr == nil {
		return "", nil
	}
	return windows.UTF16PtrToString((*uint16)(v.ptr)), nil
}

func (d *device) activate(iid *windows.GUID, out unsafe.Pointer) error {
	return d.call(3, uintptr(unsafe.Pointer(iid)), clsctxAll, 0, uintptr(out))
}

func (d *device) audioClient() (*audioClient, error) {
	var c *audioClient
	if err := d.activate(&iidIAudioClient, unsafe.Pointer(&c)); err != nil {
		return nil, err
	}
	return c, nil
}

func (d *device) endpointVolume() (*endpointVolume, error) {
	var v *endpointVolume
	if err := d.activate(&iidIAudioEndpointVolume, unsafe.Pointer(&v)); err != nil {
		return nil, err
	}
	return v, nil
}

// IAudioClient
type audioClient struct{ comObj }

func (c *audioClient) initialize(flags uint32, bufferDuration int64, wf *waveFormat) error {
	return c.call(3, audclntSharemodeShared, uintptr(flags), uintptr(bufferDuration), 0, uintptr(unsafe.Pointer(wf)), 0)
}

func (c *audioClient) mixFormat() (waveFormat, error) {
	var p *waveFormat
	if err := c.call(8, uintptr(unsafe.Pointer(&p))); err != nil {
		return waveFormat{}, err
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(p))
	// Read the 18-byte WAVEFORMATEX first; the extension is only there if
	// the tag says so.
	var wf waveFormat
	const baseSize = 18
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&wf)), baseSize), unsafe.Slice((*byte)(unsafe.Pointer(p)), baseSize))
	if wf.FormatTag == waveFormatExtensible && wf.CbSize >= 22 {
		wf = *p
	}
	return wf, nil
}

func (c *audioClient) start() error { return c.call(10) }
func (c *audioClient) stop() error  { return c.call(11) }

func (c *audioClient) captureClient() (*captureClient, error) {
	var cc *captureClient
	if err := c.call(14, uintptr(unsafe.Pointer(&iidIAudioCaptureClient)), uintptr(unsafe.Pointer(&cc))); err != nil {
		return nil, err
	}
	return cc, nil
}

// IAudioCaptureClient
type captureClient struct{ comObj }

func (c *captureClient) getBuffer() (data *byte, frames, flags uint32, err error) {
	err = c.call(3, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&frames)), uintptr(unsafe.Pointer(&flags)), 0, 0)
	return
}

func (c *captureClient) releaseBuffer(frames uint32) error { return c.call(4, uintptr(frames)) }

func (c *captureClient) nextPacketSize() (uint32, error) {
	var n uint32
	err := c.call(5, uintptr(unsafe.Pointer(&n)))
	return n, err
}

// IAudioEndpointVolume
type endpointVolume struct{ comObj }

// levelDB is the master volume as attenuation in dB (0 is full volume).
func (v *endpointVolume) levelDB() (float32, error) {
	var db float32
	err := v.call(8, uintptr(unsafe.Pointer(&db)))
	return db, err
}

func (v *endpointVolume) muted() (bool, error) {
	var m int32
	err := v.call(15, uintptr(unsafe.Pointer(&m)))
	return m != 0, err
}
