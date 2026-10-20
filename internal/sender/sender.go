// Package sender streams a Source to a receiver, reconnecting whenever the
// connection drops.
package sender

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thomasreichmann/outloud/internal/mdns"
	"github.com/thomasreichmann/outloud/internal/pairing"
	"github.com/thomasreichmann/outloud/internal/proto"
	"github.com/thomasreichmann/outloud/internal/source"
	"github.com/thomasreichmann/outloud/internal/version"
)

const (
	dialTimeout   = 3 * time.Second
	handshakeWait = 5 * time.Second
	readTimeout   = 5 * time.Second // the receiver reports stats every second
	writeTimeout  = 3 * time.Second
	pingInterval  = time.Second
	browseTime    = 1500 * time.Millisecond
	maxBackoff    = 10 * time.Second
	// eventQueue bounds how much audio waits while the network is stalled:
	// 300 ten-millisecond packets.
	eventQueue = 300
)

type State int

const (
	Connecting State = iota
	Connected
	Disconnected
)

func (s State) String() string {
	switch s {
	case Connecting:
		return "connecting"
	case Connected:
		return "connected"
	}
	return "disconnected"
}

// Status describes the sender for display.
type Status struct {
	State    State
	Receiver string // the receiver's name
	Software string // the receiver's software and version
	PeerID   string // the ID of the receiver's key
	Addr     string
	Device   string // what is being captured
	Rate     int
	Playing  bool  // audio is flowing
	Err      error // why the connection failed or capture is not running
	Stats    proto.Stats
}

// Target says which receiver to stream to.
type Target struct {
	// Name is the receiver's advertised name. Empty means the first
	// receiver discovered, unless Addr is set.
	Name string
	// Addr is host[:port]. With Name it is the fallback used when the
	// receiver cannot be discovered; on its own it is dialled directly.
	Addr string
}

// Resolve turns a Target into an address to dial.
func Resolve(ctx context.Context, t Target) (string, error) {
	if t.Name == "" && t.Addr != "" {
		return WithPort(t.Addr), nil
	}
	svcs, err := mdns.Browse(ctx, proto.ServiceType, browseTime)
	if err != nil && t.Addr == "" {
		return "", fmt.Errorf("searching for receivers: %w", err)
	}
	for _, s := range svcs {
		if t.Name == "" || strings.EqualFold(s.Instance, t.Name) {
			return s.Addr(), nil
		}
	}
	if t.Addr != "" {
		return WithPort(t.Addr), nil
	}
	if t.Name != "" {
		return "", fmt.Errorf("%q is not on the network", t.Name)
	}
	return "", errors.New("no receiver found on the network")
}

// WithPort adds the default port to an address that has none.
func WithPort(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(strings.Trim(addr, "[]"), strconv.Itoa(proto.DefaultPort))
}

type Sender struct {
	Source source.Source
	Target Target
	// Name is shown on the receiver.
	Name string
	// Identity is this device's key, and Peers the receivers paired with
	// it; the receiver must be one of them.
	Identity *pairing.Identity
	Peers    *pairing.Store
	// Latency is the buffer target to ask the receiver for; 0 uses the
	// receiver's default.
	Latency time.Duration
	// ForwardVolume applies the Windows volume on the receiver.
	ForwardVolume bool
	// TakeOver lets Run take the receiver from another device streaming to
	// it, as when the user has just chosen it. Once connected, and always
	// without TakeOver, Run waits until the receiver is free instead; the
	// status then says who uses it.
	TakeOver bool
	// OnStatus, if set, is called whenever the status changes, never
	// concurrently.
	OnStatus func(Status)

	// notify is held while a change is made and reported, so reports come
	// one at a time and in order.
	notify sync.Mutex
	mu     sync.Mutex
	status Status
	// takeOver is TakeOver until a connection succeeds.
	takeOver bool
}

// Status returns the current status.
func (s *Sender) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Sender) update(f func(*Status)) {
	s.notify.Lock()
	defer s.notify.Unlock()
	s.mu.Lock()
	f(&s.status)
	st := s.status
	s.mu.Unlock()
	if s.OnStatus != nil {
		s.OnStatus(st)
	}
}

// Run streams until ctx is cancelled. It gives up only if the receiver and
// this device are not paired, returning an error that wraps
// pairing.ErrNotPaired; the status has that error too.
func (s *Sender) Run(ctx context.Context) error {
	backoff := time.Second
	s.takeOver = s.TakeOver
	var err error
	for {
		// While the receiver is in use, the status goes on saying by whom.
		if InUseBy(err) == "" {
			s.update(func(st *Status) { *st = Status{State: Connecting} })
		}
		started := time.Now()
		var addr string
		addr, err = Resolve(ctx, s.Target)
		if err == nil {
			err = s.session(ctx, addr)
		}
		if ctx.Err() != nil {
			s.update(func(st *Status) { *st = Status{State: Disconnected} })
			return nil
		}
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		}
		s.update(func(st *Status) { *st = Status{State: Disconnected, Addr: addr, Err: err} })
		if errors.Is(err, pairing.ErrNotPaired) {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(RetryAfter(err, &backoff)):
		}
	}
}

var (
	// ErrOutdated means the receiver speaks an older protocol version, or
	// closed the connection on seeing this one, as receivers before protocol
	// version 2 do.
	ErrOutdated = errors.New("the receiver runs an older version of Outloud, which needs updating")
	// ErrNewer means the receiver speaks a newer protocol version.
	ErrNewer = errors.New("the receiver runs a newer version of Outloud; update this PC")
	// ErrNoCapture means the receiver cannot share its own sound.
	ErrNoCapture = errors.New("the receiver cannot share its sound")
	// ErrBusy means the receiver is pairing with someone else.
	ErrBusy = errors.New("the receiver is busy pairing; try again in a moment")
)

// InUseRetry is how often a sender waiting for a receiver that is in use
// asks it again.
var InUseRetry = 5 * time.Second

// InUseError means that another device, called By, is using the receiver.
type InUseError struct{ By string }

func (e *InUseError) Error() string { return "the receiver is in use by " + e.By }

// InUseBy returns who uses the receiver if err says it is in use, or "".
func InUseBy(err error) string {
	var e *InUseError
	if errors.As(err, &e) {
		return e.By
	}
	return ""
}

// RetryAfter is how long to wait before connecting again after err, given
// the backoff so far, which it updates. A receiver in use is asked again
// every InUseRetry, without backing off.
func RetryAfter(err error, backoff *time.Duration) time.Duration {
	if InUseBy(err) != "" {
		return InUseRetry
	}
	d := *backoff
	*backoff = min(2**backoff, maxBackoff)
	return d
}

// Conn is a connection to a receiver whose handshake is done.
type Conn struct {
	net.Conn // TLS
	raw      net.Conn
	Reader   *proto.Reader
	// Peer is the receiver's Hello, and PeerID the ID of its key.
	Peer   proto.Hello
	PeerID string
}

// Close drops the connection at once. It skips the TLS goodbye, which
// could wait on a stalled network for seconds.
func (c *Conn) Close() error { return c.raw.Close() }

// Dial connects to the receiver at addr and exchanges Hellos. The version
// and software of hello are filled in. Unless hello asks to pair, the
// receiver must be among peers, and this device among the receiver's paired
// devices, or the error wraps pairing.ErrNotPaired.
func Dial(ctx context.Context, addr string, hello proto.Hello, id *pairing.Identity, peers *pairing.Store) (*Conn, error) {
	d := net.Dialer{Timeout: dialTimeout, KeepAlive: 5 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Conn, error) {
		raw.Close()
		return nil, fmt.Errorf("%s: %w", addr, err)
	}
	_ = raw.SetDeadline(time.Now().Add(handshakeWait))
	if err := proto.WriteMagic(raw); err != nil {
		return fail(err)
	}
	var magic [len(proto.Magic)]byte
	if _, err := io.ReadFull(raw, magic[:]); err != nil {
		// Older receivers hang up on this version's magic.
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			err = ErrOutdated
		}
		return fail(err)
	}
	if string(magic[:]) != proto.Magic {
		return fail(fmt.Errorf("not an Outloud receiver (got %q)", magic[:]))
	}
	conn := pairing.Client(raw, id)
	if err := conn.HandshakeContext(ctx); err != nil {
		return fail(err)
	}
	peerID := pairing.PeerID(conn.ConnectionState())
	// Checked before the Hello, which would make a receiver that knows this
	// device start capturing for a listener.
	if !hello.Pair && !peers.Paired(peerID) {
		return fail(pairing.ErrNotPaired)
	}
	hello.Version, hello.Software = proto.Version, version.String()
	if err := proto.WriteJSON(conn, proto.MsgHello, hello); err != nil {
		return fail(err)
	}
	rd := proto.NewReader(bufio.NewReader(conn))
	peer, err := rd.ReadHello()
	var verr *proto.VersionError
	switch {
	case errors.As(err, &verr) && verr.Peer > proto.Version:
		return fail(ErrNewer)
	case errors.As(err, &verr):
		return fail(ErrOutdated)
	case err != nil:
		return fail(err)
	}
	switch peer.Refused {
	case "":
	case proto.RefusedNotPaired:
		return fail(pairing.ErrNotPaired)
	case proto.RefusedNoCapture:
		return fail(ErrNoCapture)
	case proto.RefusedBusy:
		return fail(ErrBusy)
	case proto.RefusedInUse:
		if peer.User == "" {
			peer.User = "another device"
		}
		return fail(&InUseError{By: peer.User})
	default:
		return fail(fmt.Errorf("the receiver refused: %s", peer.Refused))
	}
	_ = raw.SetDeadline(time.Time{})
	return &Conn{Conn: conn, raw: raw, Reader: rd, Peer: peer, PeerID: peerID}, nil
}

// Pair pairs with the receiver at addr, whether or not they are paired
// already: both sides show the same code, and each remembers the other if
// both users agree. ask shows this side's user the receiver's name and the
// code, and must return false soon after cancel is closed. It returns the
// receiver's name, and an error wrapping pairing.ErrCancelled or
// pairing.ErrDeclined if a user said no.
func Pair(ctx context.Context, addr, name string, id *pairing.Identity, peers *pairing.Store,
	ask func(receiver, code string, cancel <-chan struct{}) bool) (string, error) {
	c, err := Dial(ctx, addr, proto.Hello{Name: name, Pair: true}, id, peers)
	if err != nil {
		return "", err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(pairing.Timeout))
	code, err := pairing.Initiate(c, c.Reader, id.ID, c.PeerID)
	if err != nil {
		return c.Peer.Name, err
	}
	err = pairing.Agree(c, c.Reader, func(cancel <-chan struct{}) bool { return ask(c.Peer.Name, code, cancel) })
	if err != nil {
		return c.Peer.Name, err
	}
	return c.Peer.Name, peers.Add(pairing.Peer{ID: c.PeerID, Name: c.Peer.Name, Paired: time.Now()})
}

// session runs one connection until it fails or ctx is done.
func (s *Sender) session(ctx context.Context, addr string) error {
	hello := proto.Hello{Name: s.Name, LatencyMs: int(s.Latency / time.Millisecond), IfFree: !s.takeOver}
	c, err := Dial(ctx, addr, hello, s.Identity, s.Peers)
	if err != nil {
		return err
	}
	defer c.Close()
	s.takeOver = false
	s.update(func(st *Status) {
		*st = Status{State: Connected, Receiver: c.Peer.Name, Software: c.Peer.Software, PeerID: c.PeerID, Addr: addr}
	})
	return s.stream(ctx, c, c.Reader)
}

// Stream sends src over conn, whose handshake is done, until ctx is done,
// the connection fails or src stops; rd reads the peer's messages. This is
// how a receiver sends its own sound to a sender that listens. onStatus, if
// set, sees capture problems and the peer's stats.
func Stream(ctx context.Context, conn net.Conn, rd *proto.Reader, src source.Source, onStatus func(Status)) error {
	s := &Sender{Source: src, OnStatus: onStatus}
	s.status.State = Connected
	return s.stream(ctx, conn, rd)
}

func (s *Sender) stream(ctx context.Context, conn net.Conn, rd *proto.Reader) error {
	// Capture runs only while connected. Audio is dropped rather than
	// queued without bound if the network stalls; control events are rare
	// and must not be lost, so they wait for room.
	ctx, cancel := context.WithCancel(ctx)
	events := make(chan source.Event, eventQueue)
	srcDone := make(chan error, 1)
	srcRunning := true
	defer func() {
		// Let capture finish before a reconnect starts another.
		cancel()
		if srcRunning {
			<-srcDone
		}
	}()
	go func() {
		srcDone <- s.Source.Run(ctx, func(e source.Event) {
			if e.Kind == source.Audio {
				select {
				case events <- e:
				default:
				}
				return
			}
			select {
			case events <- e:
			case <-ctx.Done():
			}
		})
	}()

	readErr := make(chan error, 1)
	go func() {
		for {
			_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
			t, p, err := rd.Next()
			if err != nil {
				readErr <- err
				return
			}
			if t == proto.MsgStats {
				var stats proto.Stats
				if json.Unmarshal(p, &stats) == nil {
					s.update(func(st *Status) { st.Stats = stats })
				}
			}
		}
	}()

	write := func(t proto.MsgType, payload []byte) error {
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		return proto.WriteMsg(conn, t, payload)
	}
	if !s.ForwardVolume {
		if err := write(proto.MsgVolume, proto.Volume{Gain: 1}.Encode()); err != nil {
			return err
		}
	}

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	playing := false
	for {
		var err error
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return fmt.Errorf("connection lost: %w", err)
		case err := <-srcDone:
			srcRunning = false
			if err == nil {
				err = errors.New("capture stopped")
			}
			return err
		case <-ping.C:
			err = write(proto.MsgPing, nil)
		case e := <-events:
			switch e.Kind {
			case source.Format:
				err = write(proto.MsgFormat, proto.Format{SampleRate: e.Rate, Channels: 2}.Encode())
				s.update(func(st *Status) { st.Device, st.Rate, st.Err = e.Device, e.Rate, nil })
			case source.Audio:
				err = write(proto.MsgAudio, s16le(e.Samples))
				if !playing {
					playing = true
					s.update(func(st *Status) { st.Playing = true })
				}
			case source.Idle:
				if playing {
					err = write(proto.MsgIdle, nil)
					playing = false
					s.update(func(st *Status) { st.Playing = false })
				}
			case source.Volume:
				if s.ForwardVolume {
					err = write(proto.MsgVolume, proto.Volume{Gain: e.Gain}.Encode())
				}
			case source.Status:
				s.update(func(st *Status) { st.Err = e.Err })
				if e.Err != nil {
					b, _ := json.Marshal(Problem(e.Err))
					err = write(proto.MsgCapture, b)
				}
			}
		}
		if err != nil {
			return fmt.Errorf("connection lost: %w", err)
		}
	}
}

// Problem describes a capture error for the side playing the audio.
func Problem(err error) proto.Capture {
	c := proto.Capture{Error: err.Error()}
	switch {
	case errors.Is(err, source.ErrNeedsPermission):
		c.Problem = proto.CaptureNeedsPermission
	case errors.Is(err, source.ErrDenied):
		c.Problem = proto.CaptureDenied
	}
	return c
}

func s16le(samples []int16) []byte {
	b := make([]byte, 2*len(samples))
	for i, v := range samples {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}
