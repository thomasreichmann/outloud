package sender_test

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"testing"
	"time"

	"github.com/thomasreichmann/outloud/internal/pairing"
	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/proto"
	"github.com/thomasreichmann/outloud/internal/receiver"
	"github.com/thomasreichmann/outloud/internal/sender"
	"github.com/thomasreichmann/outloud/internal/source"
)

// devices is a PC and a receiver, each with its key and paired devices.
type devices struct {
	pc, mac           *pairing.Identity
	pcPeers, macPeers *pairing.Store
	srv               *receiver.Server
	addr              string
}

func identity(t *testing.T) *pairing.Identity {
	t.Helper()
	id, err := pairing.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// startReceiver runs a receiver; paired says whether it and the PC start
// out paired.
func startReceiver(t *testing.T, paired bool) (*playout.Playout, *devices) {
	t.Helper()
	d := &devices{pc: identity(t), mac: identity(t), pcPeers: pairing.MemoryStore(), macPeers: pairing.MemoryStore()}
	if paired {
		d.pcPeers.Add(pairing.Peer{ID: d.mac.ID, Name: "test receiver"})
		d.macPeers.Add(pairing.Peer{ID: d.pc.ID, Name: "test sender"})
	}
	p := playout.New(playout.Config{OutputRate: 48000, Target: 40 * time.Millisecond})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	d.srv = &receiver.Server{
		Name: "test receiver", Playout: p, Latency: 80 * time.Millisecond,
		Identity: d.mac, Peers: d.macPeers, Log: log.New(io.Discard, "", 0),
	}
	go d.srv.Serve(l)
	d.addr = l.Addr().String()
	return p, d
}

func (d *devices) sender(name string) *sender.Sender {
	return &sender.Sender{
		Source: source.Tone{Freq: 440, Gain: 0.1}, Target: sender.Target{Addr: d.addr}, Name: name,
		Identity: d.pc, Peers: d.pcPeers,
	}
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
	p, d := startReceiver(t, true)
	s := d.sender("test sender")
	s.Source = source.Tone{Freq: 440, Gain: 0.5}
	s.Latency = 40 * time.Millisecond
	s.ForwardVolume = true
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
	_, d := startReceiver(t, true)
	run := func(name string) (*sender.Sender, context.CancelFunc) {
		s := d.sender(name)
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

// runUntilDone runs s and returns what Run returned.
func runUntilDone(t *testing.T, s *sender.Sender) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.Run(ctx)
	if ctx.Err() != nil {
		t.Fatal("Run kept retrying")
	}
	return err
}

// TestNotPaired checks that a receiver and a PC that are not paired on
// either side do not connect, and that the sender gives up.
func TestNotPaired(t *testing.T) {
	_, d := startReceiver(t, false)
	// The PC does not know the receiver.
	if err := runUntilDone(t, d.sender("pc")); !errors.Is(err, pairing.ErrNotPaired) {
		t.Errorf("unknown receiver: %v", err)
	}
	// The receiver does not know the PC.
	d.pcPeers.Add(pairing.Peer{ID: d.mac.ID})
	s := d.sender("pc")
	if err := runUntilDone(t, s); !errors.Is(err, pairing.ErrNotPaired) {
		t.Errorf("unknown sender: %v", err)
	}
	if st := s.Status(); st.State != sender.Disconnected || !errors.Is(st.Err, pairing.ErrNotPaired) {
		t.Errorf("status %+v", st)
	}
}

func TestPair(t *testing.T) {
	_, d := startReceiver(t, false)
	var macCode string
	d.srv.Confirm = func(name, code string, cancel <-chan struct{}) bool {
		if name != "pc" {
			t.Errorf("the receiver was asked about %q", name)
		}
		macCode = code
		return true
	}
	var pcCode string
	mac, err := sender.Pair(context.Background(), d.addr, "pc", d.pc, d.pcPeers, func(receiver, code string, cancel <-chan struct{}) bool {
		pcCode = code
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if mac != "test receiver" || pcCode == "" || pcCode != macCode {
		t.Errorf("paired with %q, codes %q and %q", mac, pcCode, macCode)
	}
	if !d.pcPeers.Paired(d.mac.ID) || !d.macPeers.Paired(d.pc.ID) {
		t.Fatal("a side did not remember the other")
	}
	s := d.sender("pc")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, "connection after pairing", func() bool { return s.Status().State == sender.Connected })
}

func TestPairDeclined(t *testing.T) {
	_, d := startReceiver(t, false)
	d.srv.Confirm = func(name, code string, cancel <-chan struct{}) bool { return false }
	pair := func() error {
		_, err := sender.Pair(context.Background(), d.addr, "pc", d.pc, d.pcPeers, func(receiver, code string, cancel <-chan struct{}) bool {
			<-cancel
			return false
		})
		return err
	}
	if err := pair(); !errors.Is(err, pairing.ErrDeclined) {
		t.Errorf("declined: %v", err)
	}
	if d.pcPeers.Paired(d.mac.ID) || d.macPeers.Paired(d.pc.ID) {
		t.Error("a side remembered the other")
	}
	// Asking again right away is turned away.
	if err := pair(); !errors.Is(err, sender.ErrBusy) {
		t.Errorf("asked again: %v", err)
	}
}

// TestOldSender checks that a protocol 1 sender is told this receiver's
// version, so it can say what is wrong.
func TestOldSender(t *testing.T) {
	_, d := startReceiver(t, true)
	conn, err := net.Dial("tcp", d.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, proto.MagicV1)
	proto.WriteJSON(conn, proto.MsgHello, proto.Hello{Version: 1, Name: "old pc", Software: "Outloud 0.2.0"})
	br := bufio.NewReader(conn)
	magic := make([]byte, 4)
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != proto.MagicV1 {
		t.Fatalf("magic %q, %v", magic, err)
	}
	typ, p, err := proto.NewReader(br).Next()
	if err != nil || typ != proto.MsgHello {
		t.Fatalf("got %v, %v", typ, err)
	}
	var hello proto.Hello
	if err := json.Unmarshal(p, &hello); err != nil || hello.Version != proto.Version {
		t.Errorf("hello %s, %v", p, err)
	}
}

// TestOldReceiver checks that a receiver hanging up on this protocol's
// magic, as older ones do, is reported as outdated.
func TestOldReceiver(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			io.ReadFull(conn, make([]byte, 4))
			conn.Close()
		}
	}()
	_, err = sender.Dial(context.Background(), l.Addr().String(), proto.Hello{}, identity(t), pairing.MemoryStore())
	if !errors.Is(err, sender.ErrOutdated) {
		t.Errorf("got %v", err)
	}
}
