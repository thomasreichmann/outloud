// Package listener plays a receiver's own sound on this computer: it asks
// the receiver to stream what it plays and feeds that into a Playout,
// reconnecting whenever the connection drops.
package listener

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/proto"
	"github.com/thomasreichmann/outloud/internal/receiver"
	"github.com/thomasreichmann/outloud/internal/sender"
)

const maxBackoff = 10 * time.Second

// Status describes the listener for display.
type Status struct {
	State    sender.State
	Receiver string // the receiver's name
	Software string // the receiver's software and version
	Addr     string
	Err      error // why the connection failed
}

type Listener struct {
	Target sender.Target
	// Name is shown on the receiver.
	Name    string
	Playout *playout.Playout
	// Latency is the amount of audio to buffer.
	Latency time.Duration
	Log     *log.Logger
	// OnStatus, if set, is called whenever the status changes.
	OnStatus func(Status)

	mu     sync.Mutex
	status Status
}

// Status returns the current status.
func (l *Listener) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status
}

func (l *Listener) update(st Status) {
	l.mu.Lock()
	l.status = st
	l.mu.Unlock()
	if l.OnStatus != nil {
		l.OnStatus(st)
	}
}

// Run listens until ctx is cancelled.
func (l *Listener) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		l.update(Status{State: sender.Connecting})
		started := time.Now()
		addr, err := sender.Resolve(ctx, l.Target)
		if err == nil {
			err = l.session(ctx, addr)
		}
		if ctx.Err() != nil {
			l.update(Status{State: sender.Disconnected})
			return nil
		}
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		}
		l.update(Status{State: sender.Disconnected, Addr: addr, Err: err})
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

func (l *Listener) session(ctx context.Context, addr string) error {
	conn, rd, peer, err := sender.Dial(ctx, addr, proto.Hello{Name: l.Name, Listen: true})
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	l.Playout.Reset()
	l.Playout.SetTarget(l.Latency)
	defer l.Playout.Idle()
	l.update(Status{State: sender.Connected, Receiver: peer.Name, Software: peer.Software, Addr: addr})
	err = receiver.Play(conn, rd, l.Playout, func() bool { return true }, func(msg string) {
		l.Log.Printf("%s: %s", peer.Name, msg)
	})
	switch {
	case ctx.Err() != nil:
		return nil
	case errors.Is(err, io.EOF):
		return errors.New("the receiver closed the connection")
	case errors.Is(err, os.ErrDeadlineExceeded):
		return errors.New("connection lost: no data for 5 s")
	}
	return err
}
