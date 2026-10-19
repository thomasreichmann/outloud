package main

import (
	"encoding/binary"
	"math"
	"os"
	"sync"
)

// wavWriter records float32 stereo, as produced by the playout, to a 16-bit
// PCM WAV file.
type wavWriter struct {
	mu sync.Mutex
	f  *os.File
	n  uint32 // data bytes written
}

func newWAVWriter(path string, rate int) (*wavWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], 2)
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*4))
	binary.LittleEndian.PutUint16(h[32:], 4)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	if _, err := f.Write(h); err != nil {
		f.Close()
		return nil, err
	}
	return &wavWriter{f: f}, nil
}

func (w *wavWriter) Write(b []byte) (int, error) {
	out := make([]byte, len(b)/2)
	for i := 0; i+4 <= len(b); i += 4 {
		v := math.Float32frombits(binary.LittleEndian.Uint32(b[i:]))
		s := int16(math.Round(math.Max(-32768, math.Min(32767, float64(v)*32767))))
		binary.LittleEndian.PutUint16(out[i/2:], uint16(s))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return len(b), nil
	}
	n, err := w.f.Write(out)
	w.n += uint32(n)
	return len(b), err
}

// Close fills in the header's sizes.
func (w *wavWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], 36+w.n)
	w.f.WriteAt(b[:], 4)
	binary.LittleEndian.PutUint32(b[:], w.n)
	w.f.WriteAt(b[:], 40)
	err := w.f.Close()
	w.f = nil
	return err
}
