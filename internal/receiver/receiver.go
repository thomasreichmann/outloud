// Package receiver accepts a sender's connection and feeds its audio into a
// Playout.
package receiver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

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
)

// Server plays the audio of one sender at a time. A new sender takes over
// from the current one, so a sender reconnecting after a network drop never
// waits for its stale connection to time out. Independently, it streams
// Capture to one sender that listens, again with takeover.
type Server struct {
	Name    string
	Playout *playout.Playout
	// Latency is the buffer target when the sender does not ask for one.
	Latency time.Duration
	// Capture is this computer's own sound, for senders that listen; nil
	// turns them away.
	Capture source.Source
	Log     *log.Logger

	mu       sync.Mutex
	current  net.Conn
	listener net.Conn
	// captureMu is held while Capture runs, so a listener taking over
	// waits for the previous capture to stop.
	captureMu sync.Mutex
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

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	remote := conn.RemoteAddr().String()

	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	br := bufio.NewReaderSize(conn, 64<<10)
	if err := proto.ReadMagic(br); err != nil {
		s.Log.Printf("rejected connection from %s: %v", remote, err)
		return
	}
	rd := proto.NewReader(br)
	hello, err := rd.ReadHello()
	if err != nil {
		s.Log.Printf("rejected connection from %s: %v", remote, err)
		return
	}
	if hello.Listen && s.Capture == nil {
		s.Log.Printf("rejected listener at %s: this receiver cannot capture its sound", remote)
		return
	}
	if err := proto.WriteMagic(conn); err != nil {
		return
	}
	if err := proto.WriteJSON(conn, proto.MsgHello, proto.Hello{Version: proto.Version, Name: s.Name, Software: version.String()}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})

	name := hello.Name
	if name == "" {
		name = remote
	}
	software := ""
	if hello.Software != "" {
		software = " (" + hello.Software + ")"
	}
	if hello.Listen {
		s.Log.Printf("%s%s is listening from %s", name, software, remote)
		s.sendCapture(conn, rd, name)
		return
	}

	s.mu.Lock()
	if s.current != nil {
		s.current.Close()
	}
	s.current = conn
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
		if s.current == conn {
			s.current = nil
			s.Playout.Idle()
		}
		s.mu.Unlock()
	}()

	err = Play(conn, rd, s.Playout, func() bool { return s.isCurrent(conn) }, func(msg string) {
		s.Log.Printf("%s: %s", name, msg)
	})
	s.Log.Printf("%s disconnected: %s", name, describe(err, s.isCurrent(conn)))
}

// sendCapture streams Capture to a sender that listens, taking over from
// the previous one.
func (s *Server) sendCapture(conn net.Conn, rd *proto.Reader, name string) {
	s.mu.Lock()
	if s.listener != nil {
		s.listener.Close()
	}
	s.listener = conn
	s.mu.Unlock()
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	isListener := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.listener == conn
	}
	if !isListener() {
		return
	}
	defer func() {
		s.mu.Lock()
		if s.listener == conn {
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
	}
	return err.Error()
}
