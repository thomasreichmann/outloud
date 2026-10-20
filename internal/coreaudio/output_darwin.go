package coreaudio

import (
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego/objc"
)

const (
	scopeOutput = 0x6f757470 // 'outp'
	propMute    = 0x6d757465 // 'mute'
	propVolume  = 0x766f6c6d // 'volm'
)

// Output describes the default output device: its name, and whether it is
// muted or at zero volume. Devices without those controls, such as most
// HDMI outputs, count as not muted.
func Output() (name string, muted bool, err error) {
	if err := loadBase(); err != nil {
		return "", false, err
	}
	dev, err := defaultOutput()
	if err != nil {
		return "", false, err
	}
	name = deviceName(dev)
	var mute uint32
	if outputProperty(dev, propMute, elementMain, unsafe.Pointer(&mute)) == nil && mute != 0 {
		return name, true, nil
	}
	// The volume is on the device as a whole, or on each channel.
	for _, el := range []uint32{elementMain, 1} {
		var vol float32
		if outputProperty(dev, propVolume, el, unsafe.Pointer(&vol)) == nil {
			return name, vol < 0.001, nil
		}
	}
	return name, false, nil
}

func deviceName(dev uint32) string {
	runtime.LockOSThread() // autorelease pools are per thread
	defer runtime.UnlockOSThread()
	pool := objc.ID(objc.GetClass("NSAutoreleasePool")).Send(sel["new"])
	defer pool.Send(sel["drain"])
	var name objc.ID // a CFStringRef we own
	if getProperty(dev, propName, nil, 0, unsafe.Pointer(&name), 8) != nil || name == 0 {
		return ""
	}
	defer name.Send(sel["release"])
	return goString(name)
}

// outputProperty reads a 4-byte property of an output device.
func outputProperty(dev, selector, element uint32, out unsafe.Pointer) error {
	addr := propertyAddress{selector: selector, scope: scopeOutput, element: element}
	size := uint32(4)
	if st := audioObjectGetPropertyData(dev, &addr, 0, nil, &size, out); st != 0 {
		return osStatus(st)
	}
	return nil
}
