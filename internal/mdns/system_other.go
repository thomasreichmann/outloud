//go:build !windows

package mdns

import (
	"context"
	"time"
)

// systemBrowse has no system service to ask outside Windows; the sender only
// runs on Windows, and builds for other systems are for testing.
func systemBrowse(context.Context, string, time.Duration) (*records, bool) { return nil, false }
