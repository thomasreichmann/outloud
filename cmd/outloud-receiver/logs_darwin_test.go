package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestTrimLog runs trimLog in a child whose output goes to a log that has
// outgrown maxLog, as launchd's does, and in one whose output goes
// elsewhere, as when run by hand.
func TestTrimLog(t *testing.T) {
	if os.Getenv("OUTLOUD_TRIM_CHILD") != "" {
		trimLog()
		fmt.Fprintln(os.Stderr, "after trimming")
		os.Exit(0)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "Library", "Logs", "Outloud.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	old := bytes.Repeat([]byte("old line\n"), maxLog/9+1)
	// child runs trimLog with its output going to out.
	child := func(out string) {
		t.Helper()
		f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		cmd := exec.Command(os.Args[0], "-test.run=^TestTrimLog$")
		cmd.Env = append(os.Environ(), "OUTLOUD_TRIM_CHILD=1")
		cmd.Stdout, cmd.Stderr = f, f
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(path, old, 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(home, "terminal.txt")
	child(other)
	if b, _ := os.ReadFile(path); !bytes.Equal(b, old) {
		t.Errorf("trimmed the log of a receiver run by hand, which leaves %d bytes", len(b))
	}

	child(path)
	if b, _ := os.ReadFile(path + ".old"); !bytes.Equal(b, old) {
		t.Errorf("Outloud.log.old has %d bytes, want the %d of the old log", len(b), len(old))
	}
	if b, _ := os.ReadFile(path); string(b) != "after trimming\n" {
		t.Errorf("the new log has %q", b)
	}
}
