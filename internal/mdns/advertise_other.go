//go:build !darwin

package mdns

import "errors"

// Advertise is only implemented on macOS, where the receiver runs. Elsewhere
// senders have to be given the receiver's address.
func Advertise(name, service string, port int, txt []string) (stop func(), err error) {
	return nil, errors.New("service advertising is only implemented on macOS")
}
