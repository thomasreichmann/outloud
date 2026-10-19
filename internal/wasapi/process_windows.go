package wasapi

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Process loopback captures what every process plays except one process
// tree: here, this one, so that a PC playing a Mac's sound does not send it
// back. It needs Windows 10 build 20348 or later.

var (
	mmdevapi                        = windows.NewLazySystemDLL("mmdevapi.dll")
	procActivateAudioInterfaceAsync = mmdevapi.NewProc("ActivateAudioInterfaceAsync")

	iidIUnknown                   = windows.GUID{Data1: 0x00000000, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIActivateCompletionHandler = windows.GUID{Data1: 0x41D949AB, Data2: 0x9862, Data3: 0x444A, Data4: [8]byte{0x80, 0xF6, 0xC2, 0x61, 0x33, 0x4D, 0xA5, 0xEB}}
	iidIAgileObject               = windows.GUID{Data1: 0x94EA2B94, Data2: 0xE9CC, Data3: 0x49E0, Data4: [8]byte{0xC0, 0xFF, 0xEE, 0x64, 0xCA, 0x8F, 0x5B, 0x90}}

	// The vtable of completionHandler. Callbacks are never freed, so there is
	// one for all activations.
	handlerVtbl = sync.OnceValue(func() *[4]uintptr {
		return &[4]uintptr{
			windows.NewCallback(handlerQueryInterface),
			windows.NewCallback(handlerAddRef),
			windows.NewCallback(handlerRelease),
			windows.NewCallback(handlerActivateCompleted),
		}
	})

	// stuck keeps handlers whose activation timed out reachable, since
	// Windows may still call them.
	stuckMu sync.Mutex
	stuck   []*completionHandler
)

const (
	processLoopbackPath = `VAD\Process_Loopback`
	vtBlob              = 65

	activationTypeProcessLoopback = 1
	processLoopbackModeExclude    = 1

	streamflagsEventCallback = 0x00040000

	eNoInterface   = 0x80004002
	activationWait = 5 * time.Second
)

// completionHandler is an IActivateAudioInterfaceCompletionHandler, which
// must also be agile: Windows calls it from a thread of its own.
type completionHandler struct {
	vtbl *[4]uintptr
	done chan struct{}
}

func handlerQueryInterface(h *completionHandler, iid *windows.GUID, out *uintptr) uintptr {
	switch *iid {
	case iidIUnknown, iidIActivateCompletionHandler, iidIAgileObject:
		*out = uintptr(unsafe.Pointer(h))
		return 0
	}
	*out = 0
	return eNoInterface
}

// The handler's lifetime is managed in Go, so reference counts are moot.
func handlerAddRef(*completionHandler) uintptr  { return 1 }
func handlerRelease(*completionHandler) uintptr { return 1 }

func handlerActivateCompleted(h *completionHandler, _ uintptr) uintptr {
	close(h.done)
	return 0
}

// blobVariant is a PROPVARIANT holding a BLOB.
type blobVariant struct {
	vt   uint16
	_    [3]uint16
	size uint32
	data unsafe.Pointer
}

type processLoopbackParams struct {
	activationType uint32
	targetPID      uint32
	mode           uint32
}

// processLoopbackClient activates an audio client for everything this
// computer plays except the sound of this process and its children.
func processLoopbackClient() (*audioClient, error) {
	if err := procActivateAudioInterfaceAsync.Find(); err != nil {
		return nil, err
	}
	params := &processLoopbackParams{
		activationType: activationTypeProcessLoopback,
		targetPID:      uint32(os.Getpid()),
		mode:           processLoopbackModeExclude,
	}
	pv := &blobVariant{vt: vtBlob, size: uint32(unsafe.Sizeof(*params)), data: unsafe.Pointer(params)}
	h := &completionHandler{vtbl: handlerVtbl(), done: make(chan struct{})}
	path, _ := windows.UTF16PtrFromString(processLoopbackPath)

	var op *comObj // IActivateAudioInterfaceAsyncOperation
	hr, _, _ := procActivateAudioInterfaceAsync.Call(
		uintptr(unsafe.Pointer(path)),
		uintptr(unsafe.Pointer(&iidIAudioClient)),
		uintptr(unsafe.Pointer(pv)),
		uintptr(unsafe.Pointer(h)),
		uintptr(unsafe.Pointer(&op)))
	if int32(hr) < 0 {
		return nil, HRESULT(hr)
	}
	defer op.Release()
	select {
	case <-h.done:
	case <-time.After(activationWait):
		stuckMu.Lock()
		stuck = append(stuck, h)
		stuckMu.Unlock()
		return nil, errors.New("activation timed out")
	}
	runtime.KeepAlive(pv)
	runtime.KeepAlive(params)

	var result uint32
	var unk *comObj
	if err := op.call(3, uintptr(unsafe.Pointer(&result)), uintptr(unsafe.Pointer(&unk))); err != nil {
		return nil, err
	}
	if int32(result) < 0 {
		return nil, HRESULT(result)
	}
	defer unk.Release()
	var c *audioClient
	if err := unk.call(0, uintptr(unsafe.Pointer(&iidIAudioClient)), uintptr(unsafe.Pointer(&c))); err != nil {
		return nil, err
	}
	return c, nil
}

// initializeExcludingSelf sets up process loopback capture in stereo float at
// rate Hz. The returned event must be closed after the client is released.
func initializeExcludingSelf(rate uint32) (*audioClient, windows.Handle, error) {
	client, err := processLoopbackClient()
	if err != nil {
		return nil, 0, fmt.Errorf("process loopback: %w", err)
	}
	want := waveFormat{
		FormatTag:      waveFormatExtensible,
		Channels:       2,
		SamplesPerSec:  rate,
		AvgBytesPerSec: rate * 8,
		BlockAlign:     8,
		BitsPerSample:  32,
		CbSize:         22,
		ValidBits:      32,
		ChannelMask:    0x3,
		SubFormat:      subtypeIEEEFloat,
	}
	// Process loopback is event driven. The capture loop polls anyway, so
	// the event is only there because Windows requires one.
	flags := uint32(streamflagsLoopback | streamflagsEventCallback | streamflagsAutoConvertPCM | streamflagsSrcDefaultQuality)
	if err := client.initialize(flags, int64(bufferDuration/100), &want); err != nil {
		client.Release()
		return nil, 0, fmt.Errorf("process loopback: %w", err)
	}
	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		client.Release()
		return nil, 0, err
	}
	if err := client.call(13, uintptr(event)); err != nil { // SetEventHandle
		client.Release()
		windows.CloseHandle(event)
		return nil, 0, fmt.Errorf("process loopback: %w", err)
	}
	return client, event, nil
}
