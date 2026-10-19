package mdns

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"sync"

	"github.com/ebitengine/purego"
)

var (
	loadOnce sync.Once
	loadErr  error

	dnsServiceRegister func(sdRef *uintptr, flags, ifIndex uint32, name, regtype string,
		domain, host *byte, port, txtLen uint16, txt *byte, callback, context uintptr) int32
	dnsServiceRefDeallocate func(sdRef uintptr)
)

func loadDNSSD() error {
	lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return err
	}
	for name, fn := range map[string]any{
		"DNSServiceRegister":      &dnsServiceRegister,
		"DNSServiceRefDeallocate": &dnsServiceRefDeallocate,
	} {
		sym, err := purego.Dlsym(lib, name)
		if err != nil {
			return err
		}
		purego.RegisterFunc(fn, sym)
	}
	return nil
}

// Advertise registers a service with the system's mDNSResponder until stop
// is called or the process exits. An empty name means the computer name.
func Advertise(name, service string, port int, txt []string) (stop func(), err error) {
	loadOnce.Do(func() { loadErr = loadDNSSD() })
	if loadErr == nil {
		if stop, err := register(name, service, port, txt); err == nil {
			return stop, nil
		}
	}
	// Fall back to the dns-sd tool, which does the same from a child process.
	args := []string{"-R", name, service, "local", strconv.Itoa(port)}
	cmd := exec.Command("/usr/bin/dns-sd", append(args, txt...)...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("advertising %s: %w", service, err)
	}
	return func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }, nil
}

func register(name, service string, port int, txt []string) (func(), error) {
	rec := encodeTXT(txt)
	var txtPtr *byte
	if len(rec) > 0 {
		txtPtr = &rec[0]
	}
	// The port goes in network byte order; both macOS architectures are
	// little-endian.
	p := uint16(port)
	var ref uintptr
	// A nil callback tells mDNSResponder not to send replies we would have
	// to process; the registration lives as long as ref.
	rc := dnsServiceRegister(&ref, 0, 0, name, service, nil, nil, p<<8|p>>8, uint16(len(rec)), txtPtr, 0, 0)
	runtime.KeepAlive(rec)
	if rc != 0 {
		return nil, fmt.Errorf("DNSServiceRegister: error %d", rc)
	}
	return func() { dnsServiceRefDeallocate(ref) }, nil
}

// encodeTXT builds TXT record data: length-prefixed strings.
func encodeTXT(txt []string) []byte {
	var b []byte
	for _, s := range txt {
		if len(s) > 255 {
			s = s[:255]
		}
		b = append(b, byte(len(s)))
		b = append(b, s...)
	}
	return b
}
