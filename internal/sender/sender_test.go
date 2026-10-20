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
	"os"
	"sync"
	"testing"
	"time"

	"github.com/thomasreichmann/outloud/internal/pairing"
	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/proto"
	"github.com/thomasreichmann/outloud/internal/receiver"
	"github.com/thomasreichmann/outloud/internal/sender"
	"github.com/thomasreichmann/outloud/internal/source"
)

func TestMain(m *testing.M) {
	// Set before any sender runs, which reads it.
	sender.InUseRetry = 100 * time.Millisecond
	os.Exit(m.Run())
}

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
	d.srv.Output = func() receiver.Output { return receiver.Output{Device: "Test Speakers", Muted: true} }
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
	if st := s.Status().Stats; st.Output != "Test Speakers" || !st.Muted {
		t.Errorf("the receiver's output: %+v", st)
	}
}

// anotherPC pairs one more PC with the receiver, and returns a sender for it.
func (d *devices) anotherPC(t *testing.T, name string) *sender.Sender {
	t.Helper()
	id := identity(t)
	d.macPeers.Add(pairing.Peer{ID: id.ID, Name: name})
	s := d.sender(name)
	s.Identity, s.Peers = id, pairing.MemoryStore(pairing.Peer{ID: d.mac.ID, Name: "test receiver"})
	return s
}

func run(s *sender.Sender) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	return cancel
}

func connected(s *sender.Sender) func() bool {
	return func() bool { return s.Status().State == sender.Connected }
}

func waiting(s *sender.Sender, by string) func() bool {
	return func() bool {
		st := s.Status()
		return st.State == sender.Disconnected && sender.InUseBy(st.Err) == by
	}
}

// TestTakeOver checks that a PC whose user chooses a receiver takes it from
// the PC using it, which then waits, knowing who has the receiver, rather
// than take it back; and that the waiting PC continues once it is free.
func TestTakeOver(t *testing.T) {
	_, d := startReceiver(t, true)
	// Its user chose the receiver too, but that was before it connected.
	first := d.sender("first")
	first.TakeOver = true
	defer run(first)()
	waitFor(t, "first connection", connected(first))

	// A PC that starts on its own waits.
	second := d.anotherPC(t, "second")
	stop2 := run(second)
	waitFor(t, "second to wait", waiting(second, "first"))
	stop2()

	third := d.anotherPC(t, "third")
	third.TakeOver = true
	var mu sync.Mutex
	drops, up := 0, false
	third.OnStatus = func(st sender.Status) {
		mu.Lock()
		defer mu.Unlock()
		if up && st.State != sender.Connected {
			drops++
		}
		up = st.State == sender.Connected
	}
	stop3 := run(third)
	waitFor(t, "third connection", connected(third))
	waitFor(t, "first to wait", waiting(first, "third"))
	time.Sleep(10 * sender.InUseRetry)
	mu.Lock()
	n := drops
	mu.Unlock()
	if n != 0 || !waiting(first, "third")() {
		t.Fatalf("the third was dropped %d times; first: %+v", n, first.Status())
	}

	stop3()
	waitFor(t, "first to continue", connected(first))
}

// TestReconnectReplacesStale checks that a PC reconnecting on its own gets
// in although the receiver still holds its old connection, as after a
// network drop.
func TestReconnectReplacesStale(t *testing.T) {
	_, d := startReceiver(t, true)
	stale, err := sender.Dial(context.Background(), d.addr, proto.Hello{Name: "pc"}, d.pc, d.pcPeers)
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close()
	s := d.sender("pc")
	defer run(s)()
	waitFor(t, "the reconnection", connected(s))
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
