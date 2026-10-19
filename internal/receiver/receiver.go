// Package receiver accepts a sender's connection and feeds its audio into a
// Playout.
package receiver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/thomasreichmann/outloud/internal/pairing"
	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/proto"
	"github.com/thomasreichmann/outloud/internal/sender"
	"github.com/thomasreichmann/outloud/internal/source"
	"github.com/thomasreichmann/outloud/internal/version"
)

const (
	handshakeTimeout = 5 * time.Second
	// readTimeout must comfortably exceed the sender's 1 s ping interval.
	readTimeout   = 5 * time.Second
	writeTimeout  = 2 * time.Second
	statsInterval = time.Second
	// pairCooldown is how long requests to pair are turned away after one
	// was declined here, so nobody can keep asking.
	pairCooldown = 10 * time.Second
	// pairCheck is how often a connected sender's pairing is checked again.
	pairCheck = 5 * time.Second
	// repeatLog is how often the same refusal is logged; senders retry
	// every few seconds.
	repeatLog = 10 * time.Minute
)

// Server plays the audio of one sender at a time. A new sender takes over
// from the current one, so a sender reconnecting after a network drop never
// waits for its stale connection to time out. Independently, it streams
// Capture to one sender that listens, again with takeover. Only paired
// senders get either.
type Server struct {
	Name    string
	Playout *playout.Playout
	// Latency is the buffer target when the sender does not ask for one.
	Latency time.Duration
	// Capture is this computer's own sound, for senders that listen; nil
	// turns them away.
	Capture source.Source
	// Identity is this receiver's key, and Peers the senders paired with
	// it.
	Identity *pairing.Identity
	Peers    *pairing.Store
	// Confirm asks this computer's user whether to pair with the sender
	// called name, which shows code. It must return false soon after cancel
	// is closed. Nil declines every request.
	Confirm func(name, code string, cancel <-chan struct{}) bool
	Log     *log.Logger

	// current, listener and the connections in handle are the TCP
	// connections under TLS, which close at once.
	mu       sync.Mutex
	current  net.Conn
	listener net.Conn
	// captureMu is held while Capture runs, so a listener taking over
	// waits for the previous capture to stop.
	captureMu sync.Mutex
	// pairMu is held while pairing, so people are asked one at a time.
	pairMu     sync.Mutex
	declinedAt time.Time
	logged     map[string]time.Time
}

func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handle(conn)
	}
}

func (s *Server) isCurrent(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current == c
}

func (s *Server) handle(raw net.Conn) {
	defer raw.Close()
	if tc, ok := raw.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	remote := raw.RemoteAddr().String()

	_ = raw.SetDeadline(time.Now().Add(handshakeTimeout))
	var magic [len(proto.Magic)]byte
	if _, err := io.ReadFull(raw, magic[:]); err != nil {
		s.Log.Printf("rejected connection from %s: %v", remote, err)
		return
	}
	switch string(magic[:]) {
	case proto.Magic:
	case proto.MagicV1:
		s.refuseV1(raw)
		return
	default:
		s.Log.Printf("rejected connection from %s: not an Outloud peer (got %q)", remote, magic[:])
		return
	}
	if err := proto.WriteMagic(raw); err != nil {
		return
	}
	conn := pairing.Server(raw, s.Identity)
	if err := conn.Handshake(); err != nil {
		s.Log.Printf("rejected connection from %s: %v", remote, err)
		return
	}
	peerID := pairing.PeerID(conn.ConnectionState())
	rd := proto.NewReader(bufio.NewReaderSize(conn, 64<<10))
	hello, err := rd.ReadHello()
	if errors.Is(err, io.EOF) {
		// A sender hangs up here when it has not paired with this key.
		s.logOnce("hangup "+peerID, "%s hung up after seeing this computer's key: it has not paired with it", remote)
		return
	}
	if err != nil {
		s.Log.Printf("rejected connection from %s: %v", remote, err)
		return
	}
	name := hello.Name
	if name == "" {
		name = remote
	}
	refuse := func(reason string) { _ = proto.WriteJSON(conn, proto.MsgHello, s.hello(reason)) }
	switch {
	case hello.Pair:
		s.pair(conn, rd, name, remote, peerID)
		return
	case !s.Peers.Paired(peerID):
		s.logOnce("unpaired "+peerID, "turned away %s at %s: not paired with this computer", name, remote)
		refuse(proto.RefusedNotPaired)
		return
	case hello.Listen && s.Capture == nil:
		s.Log.Printf("rejected listener %s at %s: this receiver cannot capture its sound", name, remote)
		refuse(proto.RefusedNoCapture)
		return
	}
	if err := proto.WriteJSON(conn, proto.MsgHello, s.hello("")); err != nil {
		return
	}
	_ = raw.SetDeadline(time.Time{})
	done := make(chan struct{})
	defer close(done)
	go s.watchPairing(raw, name, peerID, done)

	software := ""
	if hello.Software != "" {
		software = " (" + hello.Software + ")"
	}
	if hello.Listen {
		s.Log.Printf("%s%s is listening from %s", name, software, remote)
		s.sendCapture(raw, conn, rd, name)
		return
	}

	s.mu.Lock()
	if s.current != nil {
		s.current.Close()
	}
	s.current = raw
	s.Playout.Reset()
	target := s.Latency
	if hello.LatencyMs > 0 {
		target = time.Duration(hello.LatencyMs) * time.Millisecond
	}
	s.Playout.SetTarget(target)
	s.mu.Unlock()

	s.Log.Printf("%s%s connected from %s, buffering %v", name, software, remote, target)
	defer func() {
		s.mu.Lock()
		if s.current == raw {
			s.current = nil
			s.Playout.Idle()
		}
		s.mu.Unlock()
	}()

	err = Play(conn, rd, s.Playout, func() bool { return s.isCurrent(raw) }, func(msg string) {
		s.Log.Printf("%s: %s", name, msg)
	})
	s.Log.Printf("%s disconnected: %s", name, describe(err, s.isCurrent(raw)))
}

// watchPairing drops raw, the connection of the sender called name, within
// pairCheck of it being forgotten, until done is closed.
func (s *Server) watchPairing(raw net.Conn, name, peerID string, done <-chan struct{}) {
	tick := time.NewTicker(pairCheck)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
		}
		if !s.Peers.Paired(peerID) {
			s.Log.Printf("%s is no longer paired; dropping its connection", name)
			raw.Close()
			return
		}
	}
}

func (s *Server) hello(refused string) proto.Hello {
	return proto.Hello{Version: proto.Version, Name: s.Name, Software: version.String(), Refused: refused}
}

// pair asks this computer's user whether to pair with a sender, while the
// sender's user is asked too, both seeing the same code. The sender is
// remembered if both agree.
func (s *Server) pair(conn net.Conn, rd *proto.Reader, name, remote, peerID string) {
	if !s.pairMu.TryLock() {
		s.Log.Printf("%s at %s asked to pair during another pairing", name, remote)
		_ = proto.WriteJSON(conn, proto.MsgHello, s.hello(proto.RefusedBusy))
		return
	}
	defer s.pairMu.Unlock()
	s.mu.Lock()
	recent := time.Since(s.declinedAt) < pairCooldown
	s.mu.Unlock()
	if recent {
		s.Log.Printf("%s at %s asked to pair right after a refusal", name, remote)
		_ = proto.WriteJSON(conn, proto.MsgHello, s.hello(proto.RefusedBusy))
		return
	}
	if err := proto.WriteJSON(conn, proto.MsgHello, s.hello("")); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Now().Add(pairing.Timeout))
	code, err := pairing.Respond(conn, rd, s.Identity.ID, peerID)
	if err != nil {
		s.Log.Printf("pairing with %s at %s failed: %v", name, remote, err)
		return
	}
	s.Log.Printf("%s at %s asks to pair, showing code %s", name, remote, code)
	err = pairing.Agree(conn, rd, func(cancel <-chan struct{}) bool {
		return s.Confirm != nil && s.Confirm(name, code, cancel)
	})
	switch {
	case err == nil:
		if err := s.Peers.Add(pairing.Peer{ID: peerID, Name: name, Paired: time.Now()}); err != nil {
			s.Log.Printf("pairing with %s: %v", name, err)
			return
		}
		s.Log.Printf("paired with %s", name)
	case errors.Is(err, pairing.ErrCancelled):
		s.mu.Lock()
		s.declinedAt = time.Now()
		s.mu.Unlock()
		s.Log.Printf("did not pair with %s: not allowed here", name)
	case errors.Is(err, pairing.ErrDeclined):
		s.Log.Printf("did not pair with %s: cancelled there", name)
	default:
		s.Log.Printf("did not pair with %s: %v", name, err)
	}
}

// refuseV1 answers a sender of protocol version 1, which has no TLS, with a
// Hello in that version's framing that announces this version, so it
// reports the mismatch instead of a dropped connection.
func (s *Server) refuseV1(conn net.Conn) {
	// Its Hello is read first, so that closing does not reset the
	// connection before it reads the answer.
	var hello proto.Hello
	if t, p, err := proto.NewReader(conn).Next(); err == nil && t == proto.MsgHello {
		_ = json.Unmarshal(p, &hello)
	}
	if _, err := io.WriteString(conn, proto.MagicV1); err == nil {
		_ = proto.WriteJSON(conn, proto.MsgHello, s.hello(""))
	}
	host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	s.logOnce("v1 "+host, "turned away %s at %s: it runs %s, which cannot pair; update it",
		hello.Name, conn.RemoteAddr(), hello.Software)
}

// logOnce logs at most every repeatLog for each key.
func (s *Server) logOnce(key, format string, args ...any) {
	s.mu.Lock()
	if s.logged == nil || len(s.logged) > 256 {
		s.logged = map[string]time.Time{}
	}
	last, seen := s.logged[key]
	if seen && time.Since(last) < repeatLog {
		s.mu.Unlock()
		return
	}
	s.logged[key] = time.Now()
	s.mu.Unlock()
	s.Log.Printf(format, args...)
}

// sendCapture streams Capture over conn to a sender that listens, taking
// over from the previous one. raw is the connection under conn.
func (s *Server) sendCapture(raw, conn net.Conn, rd *proto.Reader, name string) {
	s.mu.Lock()
	if s.listener != nil {
		s.listener.Close()
	}
	s.listener = raw
	s.mu.Unlock()
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	isListener := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.listener == raw
	}
	if !isListener() {
		return
	}
	defer func() {
		s.mu.Lock()
		if s.listener == raw {
			s.listener = nil
		}
		s.mu.Unlock()
	}()

	var device, lastErr string
	err := sender.Stream(context.Background(), conn, rd, s.Capture, func(st sender.Status) {
		if st.Device != "" && st.Device != device {
			device = st.Device
			s.Log.Printf("%s: sending this computer's sound, captured from %q at %d Hz", name, st.Device, st.Rate)
		}
		if st.Err != nil && st.Err.Error() != lastErr {
			lastErr = st.Err.Error()
			s.Log.Printf("%s: capture: %v", name, st.Err)
		}
	})
	if err == nil {
		err = io.EOF
	}
	s.Log.Printf("%s stopped listening: %s", name, describe(err, isListener()))
}

// Play feeds the audio a peer sends over conn, after the handshake, into
// p, until the connection fails or current reports that conn was replaced.
// It reports p's stats back every second, and notable events to logf.
func Play(conn net.Conn, rd *proto.Reader, p *playout.Playout, current func() bool, logf func(string)) error {
	done := make(chan struct{})
	defer close(done)
	go reportStats(conn, p, done)

	for {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		t, payload, err := rd.Next()
		if err != nil {
			return err
		}
		if !current() {
			return errReplaced
		}
		switch t {
		case proto.MsgFormat:
			f, err := proto.DecodeFormat(payload)
			if err != nil {
				return err
			}
			p.SetFormat(f.SampleRate, f.Channels)
			logf(fmt.Sprintf("receiving %d Hz, %d ch", f.SampleRate, f.Channels))
		case proto.MsgAudio:
			p.Write(payload)
		case proto.MsgIdle:
			p.Idle()
		case proto.MsgVolume:
			if v, err := proto.DecodeVolume(payload); err == nil {
				p.SetGain(v.Gain)
			}
		case proto.MsgPing:
		default:
			// Unknown messages are skipped so newer peers still work.
		}
	}
}

var errReplaced = errors.New("replaced by a new connection")

func reportStats(conn net.Conn, p *playout.Playout, done <-chan struct{}) {
	tick := time.NewTicker(statsInterval)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
		}
		st := p.Stats()
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		err := proto.WriteJSON(conn, proto.MsgStats, proto.Stats{
			State:     st.State.String(),
			BufferMs:  float64(st.Buffered) / float64(time.Millisecond),
			TargetMs:  float64(st.Target) / float64(time.Millisecond),
			DriftPPM:  st.DriftPPM,
			Underruns: st.Underruns,
			Skips:     st.Skips,
		})
		if err != nil {
			conn.Close()
			return
		}
	}
}

func describe(err error, current bool) string {
	switch {
	case !current || errors.Is(err, errReplaced):
		return errReplaced.Error()
	case errors.Is(err, io.EOF):
		return "sender closed the connection"
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "no data for 5 s"
	case errors.Is(err, net.ErrClosed):
		return "closed here"
	}
	return err.Error()
}
