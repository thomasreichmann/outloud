// Package proto defines the wire protocol between the Windows sender and the
// receiver that plays the audio.
//
// A sender opens a TCP connection to the receiver and both sides start by
// writing the 4-byte Magic. Everything after that is a stream of messages:
// either the sender streams audio to the receiver, or, when its Hello asks to
// listen, the receiver streams its own sound back with the same messages and
// the sender plays it.
//
//	type   uint8
//	length uint32, little endian
//	payload [length]byte
//
// Audio is always interleaved signed 16-bit little-endian PCM at the rate and
// channel count announced by the most recent Format message.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

const (
	// ServiceType is the DNS-SD service type receivers advertise.
	ServiceType = "_outloud._tcp"
	// DefaultPort is the TCP port receivers listen on.
	DefaultPort = 47811
	// Magic opens every connection, in both directions.
	Magic = "OUT1"
	// Version is bumped on incompatible protocol changes.
	Version = 1
	// MaxPayload bounds a single message so a corrupt stream cannot make
	// the reader allocate unbounded memory.
	MaxPayload = 1 << 20
)

type MsgType uint8

const (
	// MsgHello carries a JSON Hello. Each side sends one right after Magic.
	MsgHello MsgType = 1
	// MsgFormat carries a Format; it precedes the first Audio message and
	// is resent whenever the capture format changes.
	MsgFormat MsgType = 2
	// MsgAudio carries interleaved s16le samples.
	MsgAudio MsgType = 3
	// MsgIdle says the source stopped producing sound. The receiver plays
	// out what it has and then goes quiet without counting an underrun.
	MsgIdle MsgType = 4
	// MsgVolume carries a Volume to apply on the receiver.
	MsgVolume MsgType = 5
	// MsgPing is an empty keepalive the side sending audio writes every
	// second.
	MsgPing MsgType = 6
	// MsgStats carries JSON Stats, written every second by the side playing
	// the audio.
	MsgStats MsgType = 7
)

func (t MsgType) String() string {
	switch t {
	case MsgHello:
		return "hello"
	case MsgFormat:
		return "format"
	case MsgAudio:
		return "audio"
	case MsgIdle:
		return "idle"
	case MsgVolume:
		return "volume"
	case MsgPing:
		return "ping"
	case MsgStats:
		return "stats"
	}
	return fmt.Sprintf("msg(%d)", uint8(t))
}

// Hello introduces one side of the connection to the other.
type Hello struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	// Software names the program and its release, for logs.
	Software string `json:"software,omitempty"`
	// LatencyMs is the buffer target the sender asks the receiver to use.
	// Zero means the receiver's default. Only meaningful from the sender.
	LatencyMs int `json:"latency_ms,omitempty"`
	// Listen asks the receiver to stream the sound it plays itself, instead
	// of receiving audio. Only receivers whose TXT record has CaptureTXT
	// support it; older ones would take it for a sender. Only meaningful
	// from the sender.
	Listen bool `json:"listen,omitempty"`
}

// CaptureTXT is in the TXT record of receivers that can stream their own
// sound to a sender that listens.
const CaptureTXT = "capture=1"

// Stats is the playing side's view of the stream, reported back to the side
// sending it.
type Stats struct {
	State     string  `json:"state"`
	BufferMs  float64 `json:"buffer_ms"`
	TargetMs  float64 `json:"target_ms"`
	DriftPPM  float64 `json:"drift_ppm"`
	Underruns int     `json:"underruns"`
	Skips     int     `json:"skips"`
}

// Format describes the audio that follows.
type Format struct {
	SampleRate int
	Channels   int
}

func (f Format) Valid() bool {
	return f.SampleRate >= 8000 && f.SampleRate <= 384000 && (f.Channels == 1 || f.Channels == 2)
}

func (f Format) Encode() []byte {
	b := make([]byte, 5)
	binary.LittleEndian.PutUint32(b, uint32(f.SampleRate))
	b[4] = byte(f.Channels)
	return b
}

func DecodeFormat(b []byte) (Format, error) {
	if len(b) != 5 {
		return Format{}, fmt.Errorf("format payload is %d bytes, want 5", len(b))
	}
	f := Format{SampleRate: int(binary.LittleEndian.Uint32(b)), Channels: int(b[4])}
	if !f.Valid() {
		return Format{}, fmt.Errorf("unsupported format %d Hz × %d ch", f.SampleRate, f.Channels)
	}
	return f, nil
}

// Volume is a linear gain in [0, 1]; mute is sent as gain 0.
type Volume struct {
	Gain float32
}

func (v Volume) Encode() []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, math.Float32bits(v.Gain))
	return b
}

func DecodeVolume(b []byte) (Volume, error) {
	if len(b) != 4 {
		return Volume{}, fmt.Errorf("volume payload is %d bytes, want 4", len(b))
	}
	g := math.Float32frombits(binary.LittleEndian.Uint32(b))
	if !(g >= 0 && g <= 1) { // also rejects NaN
		return Volume{}, fmt.Errorf("volume gain %v out of range", g)
	}
	return Volume{Gain: g}, nil
}

// WriteMagic writes the connection preamble.
func WriteMagic(w io.Writer) error {
	_, err := io.WriteString(w, Magic)
	return err
}

// ReadMagic reads and checks the connection preamble.
func ReadMagic(r io.Reader) error {
	var b [len(Magic)]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return err
	}
	if string(b[:]) != Magic {
		return fmt.Errorf("not an Outloud peer (got %q)", b[:])
	}
	return nil
}

// WriteMsg writes one message. Callers serialize writes themselves.
func WriteMsg(w io.Writer, t MsgType, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("payload of %d bytes exceeds limit", len(payload))
	}
	buf := make([]byte, 5+len(payload))
	buf[0] = byte(t)
	binary.LittleEndian.PutUint32(buf[1:], uint32(len(payload)))
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

// WriteJSON writes v as the JSON payload of a message.
func WriteJSON(w io.Writer, t MsgType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteMsg(w, t, b)
}

// Reader decodes messages from a stream, reusing its payload buffer.
type Reader struct {
	r   io.Reader
	hdr [5]byte
	buf []byte
}

func NewReader(r io.Reader) *Reader { return &Reader{r: r} }

// Next returns the next message. The payload is only valid until the next
// call.
func (r *Reader) Next() (MsgType, []byte, error) {
	if _, err := io.ReadFull(r.r, r.hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.LittleEndian.Uint32(r.hdr[1:])
	if n > MaxPayload {
		return 0, nil, fmt.Errorf("message of %d bytes exceeds limit", n)
	}
	if cap(r.buf) < int(n) {
		r.buf = make([]byte, n)
	}
	r.buf = r.buf[:n]
	if _, err := io.ReadFull(r.r, r.buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return MsgType(r.hdr[0]), r.buf, nil
}

// ReadHello reads a message that must be a Hello.
func (r *Reader) ReadHello() (Hello, error) {
	t, p, err := r.Next()
	if err != nil {
		return Hello{}, err
	}
	if t != MsgHello {
		return Hello{}, fmt.Errorf("expected hello, got %v", t)
	}
	var h Hello
	if err := json.Unmarshal(p, &h); err != nil {
		return Hello{}, fmt.Errorf("bad hello: %w", err)
	}
	if h.Version != Version {
		return Hello{}, fmt.Errorf("peer speaks protocol version %d, this build speaks %d", h.Version, Version)
	}
	return h, nil
}
