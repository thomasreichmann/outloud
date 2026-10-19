package macalert

import (
	"os"
	"testing"
	"time"
)

// These tests put an alert on the Mac's screen, so they only run on request.
// From an SSH session, run them in the login session with
// "launchctl asuser $(id -u) ./macalert.test".
func needAlert(t *testing.T) {
	t.Helper()
	if os.Getenv("OUTLOUD_ALERT_TEST") == "" {
		t.Skip("set OUTLOUD_ALERT_TEST=1 to show alerts on this Mac")
	}
}

func TestCancel(t *testing.T) {
	needAlert(t)
	cancel := make(chan struct{})
	time.AfterFunc(3*time.Second, func() { close(cancel) })
	start := time.Now()
	yes, err := Ask("Outloud test", "This alert tests Outloud and closes by itself in a few seconds.",
		"Allow", "Don't Allow", time.Minute, cancel)
	if err != nil {
		t.Fatal(err)
	}
	if yes {
		t.Error("a cancelled alert reported yes")
	}
	if d := time.Since(start); d < 2*time.Second || d > 10*time.Second {
		t.Errorf("cancelling took %v after 3 s", d)
	}
}

func TestTimeout(t *testing.T) {
	needAlert(t)
	start := time.Now()
	yes, err := Ask("Outloud test", "This alert tests Outloud and closes by itself in a few seconds.",
		"Allow", "Don't Allow", 2*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if yes {
		t.Error("an alert that timed out reported yes")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("a 2 s alert took %v", d)
	}
}
