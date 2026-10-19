// Package macalert shows a standard macOS alert from a process that has no
// user interface of its own, such as the receiver's LaunchAgent.
package macalert

import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	encodingUTF8      = 0x08000100 // kCFStringEncodingUTF8
	alertLevelCaution = 2          // kCFUserNotificationCautionAlertLevel
	responseMask      = 3
	responseAlternate = 1 // kCFUserNotificationAlternateResponse
)

var (
	loadOnce sync.Once
	loadErr  error

	cfStringCreateWithBytes           func(alloc uintptr, b *byte, n int, encoding uint32, external bool) uintptr
	cfDictionaryCreate                func(alloc uintptr, keys, values *uintptr, n int, keyCallBacks, valueCallBacks uintptr) uintptr
	cfRelease                         func(cf uintptr)
	cfUserNotificationCreate          func(alloc uintptr, timeout float64, flags uintptr, err *int32, dict uintptr) uintptr
	cfUserNotificationReceiveResponse func(n uintptr, timeout float64, flags *uintptr) int32
	cfUserNotificationCancel          func(n uintptr) int32

	// Addresses of CoreFoundation's dictionary callback structs, and the
	// CFStrings it uses as alert keys.
	keyCallBacks, valueCallBacks                    uintptr
	headerKey, messageKey, defaultKey, alternateKey uintptr
)

func load() error {
	loadOnce.Do(func() {
		cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			loadErr = err
			return
		}
		for name, fn := range map[string]any{
			"CFStringCreateWithBytes":           &cfStringCreateWithBytes,
			"CFDictionaryCreate":                &cfDictionaryCreate,
			"CFRelease":                         &cfRelease,
			"CFUserNotificationCreate":          &cfUserNotificationCreate,
			"CFUserNotificationReceiveResponse": &cfUserNotificationReceiveResponse,
			"CFUserNotificationCancel":          &cfUserNotificationCancel,
		} {
			sym, err := purego.Dlsym(cf, name)
			if err != nil {
				loadErr = err
				return
			}
			purego.RegisterFunc(fn, sym)
		}
		for name, addr := range map[string]*uintptr{
			"kCFTypeDictionaryKeyCallBacks":   &keyCallBacks,
			"kCFTypeDictionaryValueCallBacks": &valueCallBacks,
		} {
			if *addr, err = purego.Dlsym(cf, name); err != nil {
				loadErr = err
				return
			}
		}
		for name, s := range map[string]*uintptr{
			"kCFUserNotificationAlertHeaderKey":          &headerKey,
			"kCFUserNotificationAlertMessageKey":         &messageKey,
			"kCFUserNotificationDefaultButtonTitleKey":   &defaultKey,
			"kCFUserNotificationAlternateButtonTitleKey": &alternateKey,
		} {
			addr, err := purego.Dlsym(cf, name)
			if err != nil {
				loadErr = err
				return
			}
			*s = **(**uintptr)(unsafe.Pointer(&addr)) // the symbol holds a CFStringRef
		}
	})
	return loadErr
}

func cfString(s string) uintptr {
	if s == "" {
		s = " "
	}
	b := []byte(s)
	return cfStringCreateWithBytes(0, &b[0], len(b), encodingUTF8, false)
}

// ErrUnavailable means no alert could be shown, for example because the
// process runs outside a login session.
var ErrUnavailable = errors.New("cannot show an alert on this Mac")

// Ask shows an alert with two buttons and waits until someone clicks one,
// timeout passes or cancel is closed. It reports whether the button labelled
// yes was clicked. The other button, labelled no, is the default, so an alert
// that appears while someone is typing is not accepted by Return.
func Ask(header, message, yes, no string, timeout time.Duration, cancel <-chan struct{}) (bool, error) {
	if err := load(); err != nil {
		return false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	keys := []uintptr{headerKey, messageKey, defaultKey, alternateKey}
	values := []uintptr{cfString(header), cfString(message), cfString(no), cfString(yes)}
	defer func() {
		for _, v := range values {
			cfRelease(v)
		}
	}()
	dict := cfDictionaryCreate(0, &keys[0], &values[0], len(keys), keyCallBacks, valueCallBacks)
	if dict == 0 {
		return false, ErrUnavailable
	}
	defer cfRelease(dict)

	var code int32
	n := cfUserNotificationCreate(0, timeout.Seconds(), alertLevelCaution, &code, dict)
	if n == 0 {
		return false, fmt.Errorf("%w (error %d)", ErrUnavailable, code)
	}
	defer cfRelease(n)
	answered := make(chan struct{})
	defer close(answered)
	go func() {
		select {
		case <-cancel:
			cfUserNotificationCancel(n)
		case <-answered:
		}
	}()
	var flags uintptr
	if r := cfUserNotificationReceiveResponse(n, 0, &flags); r != 0 {
		return false, fmt.Errorf("%w (error %d)", ErrUnavailable, r)
	}
	return flags&responseMask == responseAlternate, nil
}
