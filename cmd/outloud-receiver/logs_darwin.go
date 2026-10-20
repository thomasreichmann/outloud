package main

import (
	"os"
	"syscall"
	"time"
)

// maxLog is how large the log grows before it starts over. The one before
// is kept as Outloud.log.old.
const maxLog = 1 << 20

// trimLog keeps the log, which launchd appends this receiver's output to,
// from growing without end. If the receiver's output goes to that file and
// the file has outgrown maxLog, it becomes Outloud.log.old, and the output
// goes on in a new one. It checks now and every hour.
func trimLog() {
	p, err := installPaths()
	if err != nil {
		return
	}
	trim := func() {
		fi, err := os.Stat(p.log)
		if err != nil || fi.Size() <= maxLog {
			return
		}
		var out syscall.Stat_t
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || syscall.Fstat(2, &out) != nil || st.Dev != out.Dev || st.Ino != out.Ino {
			return // run by hand, not by launchd
		}
		if os.Rename(p.log, p.log+".old") != nil {
			return
		}
		f, err := os.OpenFile(p.log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		_ = syscall.Dup2(int(f.Fd()), 1)
		_ = syscall.Dup2(int(f.Fd()), 2)
	}
	trim()
	go func() {
		for range time.Tick(time.Hour) {
			trim()
		}
	}()
}
