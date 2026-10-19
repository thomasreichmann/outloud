package listener_test

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"math"
	"net"
	"testing"
	"time"

	"github.com/thomasreichmann/outloud/internal/listener"
	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/receiver"
	"github.com/thomasreichmann/outloud/internal/sender"
	"github.com/thomasreichmann/outloud/internal/source"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// peak pulls 20 ms from p and returns its peak level.
func peak(p *playout.Playout) float64 {
	b := make([]byte, 8*960)
	p.Read(b)
	var pk float64
	for i := 0; i < len(b); i += 8 {
		pk = math.Max(pk, math.Abs(float64(math.Float32frombits(binary.LittleEndian.Uint32(b[i:])))))
	}
	return pk
}

// TestBothDirections streams a tone to the receiver and listens to the
// receiver's own tone at the same time, over separate connections.
func TestBothDirections(t *testing.T) {
	remote := playout.New(playout.Config{OutputRate: 48000, Target: 40 * time.Millisecond})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	srv := &receiver.Server{
		Name:    "test receiver",
		Playout: remote,
		Capture: source.Tone{Freq: 660, Gain: 0.25},
		Log:     log.New(io.Discard, "", 0),
	}
	go srv.Serve(l)
	target := sender.Target{Addr: l.Addr().String()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &sender.Sender{Source: source.Tone{Freq: 440, Gain: 0.5}, Target: target, Name: "pc", Latency: 40 * time.Millisecond}
	go s.Run(ctx)
	local := playout.New(playout.Config{OutputRate: 48000, Target: 40 * time.Millisecond})
	ln := &listener.Listener{Target: target, Name: "pc", Playout: local, Latency: 40 * time.Millisecond, Log: log.New(io.Discard, "", 0)}
	go ln.Run(ctx)

	waitFor(t, "listener to connect", func() bool { return ln.Status().State == sender.Connected })
	if st := ln.Status(); st.Receiver != "test receiver" {
		t.Errorf("receiver name %q", st.Receiver)
	}
	waitFor(t, "both buffers", func() bool {
		return remote.Stats().Buffered >= 40*time.Millisecond && local.Stats().Buffered >= 40*time.Millisecond
	})
	// The tones beep half of every second; wait for both to be sounding.
	waitFor(t, "the receiver to hear the sender", func() bool { return peak(remote) > 0.4 })
	waitFor(t, "the listener to hear the receiver", func() bool { return peak(local) > 0.2 })
	if pk := peak(local); pk > 0.3 {
		t.Errorf("listener peak %.3f, want the receiver's tone at ~0.25", pk)
	}
	if s.Status().State != sender.Connected {
		t.Error("the sender was dropped when the listener connected")
	}
}

// TestNoCapture checks that a receiver without a capture source turns a
// listener away instead of taking it for a sender.
func TestNoCapture(t *testing.T) {
	remote := playout.New(playout.Config{OutputRate: 48000, Target: 40 * time.Millisecond})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	srv := &receiver.Server{Name: "old", Playout: remote, Log: log.New(io.Discard, "", 0)}
	go srv.Serve(l)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := &listener.Listener{Target: sender.Target{Addr: l.Addr().String()}, Playout: remote, Latency: 40 * time.Millisecond, Log: log.New(io.Discard, "", 0)}
	go ln.Run(ctx)
	waitFor(t, "a refusal", func() bool {
		st := ln.Status()
		return st.State == sender.Disconnected && st.Err != nil
	})
}
