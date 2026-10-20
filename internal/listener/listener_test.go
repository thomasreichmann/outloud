package listener_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/thomasreichmann/outloud/internal/listener"
	"github.com/thomasreichmann/outloud/internal/pairing"
	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/proto"
	"github.com/thomasreichmann/outloud/internal/receiver"
	"github.com/thomasreichmann/outloud/internal/sender"
	"github.com/thomasreichmann/outloud/internal/source"
)

func TestMain(m *testing.M) {
	// Set before any listener runs, which reads it.
	sender.InUseRetry = 100 * time.Millisecond
	os.Exit(m.Run())
}

// paired makes keys for a PC and a receiver that are paired with each
// other, and the receiver's list of paired devices.
func paired(t *testing.T) (pc, mac *pairing.Identity, pcPeers, macPeers *pairing.Store) {
	t.Helper()
	var err error
	if pc, err = pairing.NewIdentity(); err != nil {
		t.Fatal(err)
	}
	if mac, err = pairing.NewIdentity(); err != nil {
		t.Fatal(err)
	}
	return pc, mac, pairing.MemoryStore(pairing.Peer{ID: mac.ID}), pairing.MemoryStore(pairing.Peer{ID: pc.ID})
}

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
	pc, mac, pcPeers, macPeers := paired(t)
	srv := &receiver.Server{
		Name:     "test receiver",
		Playout:  remote,
		Capture:  source.Tone{Freq: 660, Gain: 0.25},
		Identity: mac,
		Peers:    macPeers,
		Log:      log.New(io.Discard, "", 0),
	}
	go srv.Serve(l)
	target := sender.Target{Addr: l.Addr().String()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &sender.Sender{
		Source: source.Tone{Freq: 440, Gain: 0.5}, Target: target, Name: "pc", Latency: 40 * time.Millisecond,
		Identity: pc, Peers: pcPeers,
	}
	go s.Run(ctx)
	local := playout.New(playout.Config{OutputRate: 48000, Target: 40 * time.Millisecond})
	ln := &listener.Listener{
		Target: target, Name: "pc", Identity: pc, Peers: pcPeers,
		Playout: local, Latency: 40 * time.Millisecond, Log: log.New(io.Discard, "", 0),
	}
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

// stuckCapture behaves like a Mac's capture while macOS asks for permission:
// it says so, then waits until release is closed, ignoring cancellation, as
// the call that waits for the answer does.
type stuckCapture struct{ release chan struct{} }

func (c stuckCapture) Run(ctx context.Context, emit func(source.Event)) error {
	emit(source.Event{Kind: source.Status, Err: fmt.Errorf("%w: test", source.ErrNeedsPermission)})
	<-c.release
	if ctx.Err() != nil {
		return nil
	}
	return source.Tone{Freq: 660, Gain: 0.25}.Run(ctx, emit)
}

// TestCaptureProblem checks that a listener learns why the receiver sends
// nothing, also when it waits for the previous listener's capture to stop,
// and that the sound comes once capture works.
func TestCaptureProblem(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	pc, mac, pcPeers, macPeers := paired(t)
	release := make(chan struct{})
	srv := &receiver.Server{
		Name: "test receiver", Playout: playout.New(playout.Config{OutputRate: 48000}),
		Capture: stuckCapture{release}, Identity: mac, Peers: macPeers, Log: log.New(io.Discard, "", 0),
	}
	go srv.Serve(l)
	// listen starts a listener, and returns how often its connection drops.
	listen := func() (*listener.Listener, *playout.Playout, func() int, context.CancelFunc) {
		local := playout.New(playout.Config{OutputRate: 48000, Target: 40 * time.Millisecond})
		var mu sync.Mutex
		drops, connected := 0, false
		ln := &listener.Listener{
			Target: sender.Target{Addr: l.Addr().String()}, Name: "pc", Identity: pc, Peers: pcPeers,
			Playout: local, Latency: 40 * time.Millisecond, Log: log.New(io.Discard, "", 0),
			OnStatus: func(st listener.Status) {
				mu.Lock()
				defer mu.Unlock()
				if connected && st.State != sender.Connected {
					drops++
				}
				connected = st.State == sender.Connected
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		go ln.Run(ctx)
		return ln, local, func() int { mu.Lock(); defer mu.Unlock(); return drops }, cancel
	}
	needsPermission := func(ln *listener.Listener) func() bool {
		return func() bool {
			st := ln.Status()
			return st.State == sender.Connected && st.Capture.Problem == proto.CaptureNeedsPermission
		}
	}

	first, _, _, stopFirst := listen()
	waitFor(t, "the first listener to hear why", needsPermission(first))
	// The first listener goes, leaving its capture stuck; the next one
	// waits for it.
	stopFirst()
	second, local, drops, stopSecond := listen()
	defer stopSecond()
	waitFor(t, "the second listener to hear why", needsPermission(second))
	time.Sleep(7 * time.Second) // longer than the 5 s without data that ends a connection
	if drops() != 0 || second.Status().State != sender.Connected {
		t.Fatalf("the waiting listener was dropped %d times", drops())
	}

	close(release)
	waitFor(t, "the problem to clear", func() bool { return second.Status().Capture == proto.Capture{} })
	waitFor(t, "the receiver's sound", func() bool { return peak(local) > 0.2 })
}

// TestListenTakeOver checks that, as with streaming, a PC that listens on
// its own waits while another PC listens, and one whose user chose to
// listen takes over, leaving the other to wait.
func TestListenTakeOver(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	pc, mac, pcPeers, macPeers := paired(t)
	other, err := pairing.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	macPeers.Add(pairing.Peer{ID: other.ID, Name: "other"})
	otherPeers := pairing.MemoryStore(pairing.Peer{ID: mac.ID})
	srv := &receiver.Server{
		Name: "test receiver", Playout: playout.New(playout.Config{OutputRate: 48000}),
		Capture: source.Tone{Freq: 660, Gain: 0.25}, Identity: mac, Peers: macPeers, Log: log.New(io.Discard, "", 0),
	}
	go srv.Serve(l)
	listen := func(name string, id *pairing.Identity, peers *pairing.Store, takeOver bool) (*listener.Listener, context.CancelFunc) {
		ln := &listener.Listener{
			Target: sender.Target{Addr: l.Addr().String()}, Name: name, Identity: id, Peers: peers,
			Playout: playout.New(playout.Config{OutputRate: 48000}), Latency: 40 * time.Millisecond,
			Log: log.New(io.Discard, "", 0), TakeOver: takeOver,
		}
		ctx, cancel := context.WithCancel(context.Background())
		go ln.Run(ctx)
		return ln, cancel
	}
	listening := func(ln *listener.Listener) func() bool {
		return func() bool { return ln.Status().State == sender.Connected }
	}
	waiting := func(ln *listener.Listener, by string) func() bool {
		return func() bool {
			st := ln.Status()
			return st.State == sender.Disconnected && sender.InUseBy(st.Err) == by
		}
	}

	first, stop1 := listen("first", pc, pcPeers, true)
	defer stop1()
	waitFor(t, "the first to listen", listening(first))
	second, stop2 := listen("second", other, otherPeers, false)
	waitFor(t, "the second to wait", waiting(second, "first"))
	stop2()
	third, stop3 := listen("third", other, otherPeers, true)
	defer stop3()
	waitFor(t, "the third to listen", listening(third))
	waitFor(t, "the first to wait", waiting(first, "third"))
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
	pc, mac, pcPeers, macPeers := paired(t)
	srv := &receiver.Server{Name: "no capture", Playout: remote, Identity: mac, Peers: macPeers, Log: log.New(io.Discard, "", 0)}
	go srv.Serve(l)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := &listener.Listener{
		Target: sender.Target{Addr: l.Addr().String()}, Identity: pc, Peers: pcPeers,
		Playout: remote, Latency: 40 * time.Millisecond, Log: log.New(io.Discard, "", 0),
	}
	go ln.Run(ctx)
	waitFor(t, "a refusal", func() bool {
		st := ln.Status()
		return st.State == sender.Disconnected && errors.Is(st.Err, sender.ErrNoCapture)
	})
}
