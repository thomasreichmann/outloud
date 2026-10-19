package sender_test

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"math"
	"net"
	"testing"
	"time"

	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/receiver"
	"github.com/thomasreichmann/outloud/internal/sender"
	"github.com/thomasreichmann/outloud/internal/source"
)

func startReceiver(t *testing.T) (*playout.Playout, string) {
	t.Helper()
	p := playout.New(playout.Config{OutputRate: 48000, Target: 40 * time.Millisecond})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	srv := &receiver.Server{Name: "test receiver", Playout: p, Latency: 80 * time.Millisecond, Log: log.New(io.Discard, "", 0)}
	go srv.Serve(l)
	return p, l.Addr().String()
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

func TestStreamsToReceiver(t *testing.T) {
	p, addr := startReceiver(t)
	s := &sender.Sender{
		Source:        source.Tone{Freq: 440, Gain: 0.5},
		Target:        sender.Target{Addr: addr},
		Name:          "test sender",
		Latency:       40 * time.Millisecond,
		ForwardVolume: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, "connection", func() bool { return s.Status().State == sender.Connected })
	if st := s.Status(); st.Receiver != "test receiver" {
		t.Errorf("receiver name %q", st.Receiver)
	}
	// Playback starts when the device pulls, once the target is buffered.
	waitFor(t, "buffering", func() bool { return p.Stats().Buffered >= 40*time.Millisecond })

	// Pull 20 ms from the device side and check the tone came through.
	b := make([]byte, 8*960)
	p.Read(b)
	if st := p.State(); st != playout.Playing {
		t.Fatalf("state %v after read", st)
	}
	var peak float64
	for i := 0; i < len(b); i += 8 {
		peak = math.Max(peak, math.Abs(float64(math.Float32frombits(binary.LittleEndian.Uint32(b[i:])))))
	}
	if peak < 0.4 || peak > 0.55 {
		t.Errorf("tone peak %.3f, want ~0.5", peak)
	}
	st := p.Stats()
	if st.SampleRate != 48000 || st.Target != 40*time.Millisecond {
		t.Errorf("receiver stats %+v", st)
	}
	waitFor(t, "stats from the receiver", func() bool { return s.Status().Stats.State != "" })
}

func TestNewSenderTakesOver(t *testing.T) {
	_, addr := startReceiver(t)
	run := func(name string) (*sender.Sender, context.CancelFunc) {
		s := &sender.Sender{Source: source.Tone{Freq: 440, Gain: 0.1}, Target: sender.Target{Addr: addr}, Name: name}
		ctx, cancel := context.WithCancel(context.Background())
		go s.Run(ctx)
		return s, cancel
	}
	first, stop1 := run("first")
	defer stop1()
	waitFor(t, "first connection", func() bool { return first.Status().State == sender.Connected })
	second, stop2 := run("second")
	defer stop2()
	waitFor(t, "second connection", func() bool { return second.Status().State == sender.Connected })
	waitFor(t, "first to be dropped", func() bool { return first.Status().State != sender.Connected })
}
