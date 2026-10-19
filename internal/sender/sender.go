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
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thomasreichmann/outloud/internal/mdns"
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
	// Latency is the buffer target to ask the receiver for; 0 uses the
	// receiver's default.
	Latency time.Duration
	// ForwardVolume applies the Windows volume on the receiver.
	ForwardVolume bool
	// OnStatus, if set, is called whenever the status changes.
	OnStatus func(Status)

	mu     sync.Mutex
	status Status
}

// Status returns the current status.
func (s *Sender) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Sender) update(f func(*Status)) {
	s.mu.Lock()
	f(&s.status)
	st := s.status
	s.mu.Unlock()
	if s.OnStatus != nil {
		s.OnStatus(st)
	}
}

// Run streams until ctx is cancelled.
func (s *Sender) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		s.update(func(st *Status) { *st = Status{State: Connecting} })
		started := time.Now()
		addr, err := Resolve(ctx, s.Target)
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
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

// session runs one connection until it fails or ctx is done.
func (s *Sender) session(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: dialTimeout, KeepAlive: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(handshakeWait))
	if err := proto.WriteMagic(conn); err != nil {
		return err
	}
	hello := proto.Hello{Version: proto.Version, Name: s.Name, Software: version.String(), LatencyMs: int(s.Latency / time.Millisecond)}
	if err := proto.WriteJSON(conn, proto.MsgHello, hello); err != nil {
		return err
	}
	br := bufio.NewReader(conn)
	if err := proto.ReadMagic(br); err != nil {
		return fmt.Errorf("%s: %w", addr, err)
	}
	rd := proto.NewReader(br)
	peer, err := rd.ReadHello()
	if err != nil {
		return fmt.Errorf("%s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Time{})
	s.update(func(st *Status) {
		*st = Status{State: Connected, Receiver: peer.Name, Software: peer.Software, Addr: addr}
	})

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
			}
		}
		if err != nil {
			return fmt.Errorf("connection lost: %w", err)
		}
	}
}

func s16le(samples []int16) []byte {
	b := make([]byte, 2*len(samples))
	for i, v := range samples {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}
