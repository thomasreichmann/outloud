package wasapi

import (
	"runtime"
	"testing"
)

// TestScheduling checks that this process can opt out of power throttling,
// and that registering a thread with MMCSS, where the service runs, and
// releasing it again work.
func TestScheduling(t *testing.T) {
	if err := NoThrottling(); err != nil {
		t.Error(err)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	release := audioThread()
	release()
}
