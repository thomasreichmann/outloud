package wasapi

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows runs a program that shows no window, such as a tray app, as
// background work: it may slow its timers and run it on efficiency cores to
// save power, which holds up capture for tens of milliseconds at a time.
// Audio programs opt out, and have their audio threads scheduled by MMCSS,
// the Multimedia Class Scheduler Service, which runs them promptly.

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procSetProcessInformation = kernel32.NewProc("SetProcessInformation")
	avrt                      = windows.NewLazySystemDLL("avrt.dll")
	procAvSetMmThreadChars    = avrt.NewProc("AvSetMmThreadCharacteristicsW")
	procAvRevertMmThreadChars = avrt.NewProc("AvRevertMmThreadCharacteristics")
	proAudio, _               = windows.UTF16PtrFromString("Pro Audio")
)

const (
	processPowerThrottling = 4   // PROCESS_INFORMATION_CLASS
	throttleExecutionSpeed = 0x1 // PROCESS_POWER_THROTTLING_EXECUTION_SPEED
	throttleTimers         = 0x4 // PROCESS_POWER_THROTTLING_IGNORE_TIMER_RESOLUTION
)

// NoThrottling asks Windows not to slow this process down to save power.
func NoThrottling() error {
	state := struct{ version, controlMask, stateMask uint32 }{1, throttleExecutionSpeed | throttleTimers, 0}
	r, _, err := procSetProcessInformation.Call(uintptr(windows.CurrentProcess()), processPowerThrottling,
		uintptr(unsafe.Pointer(&state)), unsafe.Sizeof(state))
	if r == 0 {
		return fmt.Errorf("opting out of power throttling: %w", err)
	}
	return nil
}

// audioThread has MMCSS schedule the calling thread, which must be locked
// to it, as audio work, until the returned function is called.
func audioThread() (release func()) {
	var index uint32
	h, _, _ := procAvSetMmThreadChars.Call(uintptr(unsafe.Pointer(proAudio)), uintptr(unsafe.Pointer(&index)))
	if h == 0 {
		return func() {}
	}
	return func() { procAvRevertMmThreadChars.Call(h) }
}
