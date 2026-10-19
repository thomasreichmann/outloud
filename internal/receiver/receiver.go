// Package receiver accepts a sender's connection and feeds its audio into a
// Playout.
package receiver

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/proto"
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
// waits for its stale connection to time out.
type Server struct {
	Name    string
	Playout *playout.Playout
	// Latency is the buffer target when the sender does not ask for one.
	Latency time.Duration
	Log     *log.Logger

	mu      sync.Mutex
	current net.Conn
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
	if err := proto.WriteMagic(conn); err != nil {
		return
	}
	if err := proto.WriteJSON(conn, proto.MsgHello, proto.Hello{Version: proto.Version, Name: s.Name, Software: version.String()}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})

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

	name := hello.Name
	if name == "" {
		name = remote
	}
	software := ""
	if hello.Software != "" {
		software = " (" + hello.Software + ")"
	}
	s.Log.Printf("%s%s connected from %s, buffering %v", name, software, remote, target)
	defer func() {
		s.mu.Lock()
		if s.current == conn {
			s.current = nil
			s.Playout.Idle()
		}
		s.mu.Unlock()
	}()

	done := make(chan struct{})
	defer close(done)
	go s.reportStats(conn, done)

	for {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		t, p, err := rd.Next()
		if err != nil {
			s.Log.Printf("%s disconnected: %s", name, describe(err, s.isCurrent(conn)))
			return
		}
		if !s.isCurrent(conn) {
			return
		}
		switch t {
		case proto.MsgFormat:
			f, err := proto.DecodeFormat(p)
			if err != nil {
				s.Log.Printf("%s: %v", name, err)
				return
			}
			s.Playout.SetFormat(f.SampleRate, f.Channels)
			s.Log.Printf("%s: receiving %d Hz, %d ch", name, f.SampleRate, f.Channels)
		case proto.MsgAudio:
			s.Playout.Write(p)
		case proto.MsgIdle:
			s.Playout.Idle()
		case proto.MsgVolume:
			if v, err := proto.DecodeVolume(p); err == nil {
				s.Playout.SetGain(v.Gain)
			}
		case proto.MsgPing:
		default:
			// Unknown messages are skipped so newer senders still work.
		}
	}
}

func (s *Server) reportStats(conn net.Conn, done <-chan struct{}) {
	tick := time.NewTicker(statsInterval)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
		}
		st := s.Playout.Stats()
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
	case !current:
		return "replaced by a new connection"
	case errors.Is(err, io.EOF):
		return "sender closed the connection"
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "no data for 5 s"
	}
	return err.Error()
}
