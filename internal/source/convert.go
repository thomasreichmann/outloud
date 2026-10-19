package source

import (
	"encoding/binary"
	"math"
	"math/bits"
)

type SampleKind int

const (
	Float32 SampleKind = iota
	Int16
	Int24 // packed, 3 bytes per sample
	Int32 // also 24-bit samples in a 32-bit container
)

func (k SampleKind) Bytes() int {
	switch k {
	case Int16:
		return 2
	case Int24:
		return 3
	}
	return 4
}

// silenceLevel is the magnitude below which a sample counts as silence
// (-100 dBFS); some apps leave a stream open playing almost-zeros.
const silenceLevel = 1e-5

// Converter turns captured frames in a device's native layout into
// interleaved stereo s16, downmixing and dithering as needed.
type Converter struct {
	kind     SampleKind
	channels int
	matrix   [][2]float32 // per input channel: contribution to left, right
	rng      uint32
}

// Speaker position bits of WAVEFORMATEXTENSIBLE.dwChannelMask, in channel
// order, with their contribution to a stereo downmix.
var speakerMix = []struct {
	bit  uint32
	l, r float32
}{
	{0x1, 1, 0},         // front left
	{0x2, 0, 1},         // front right
	{0x4, 0.707, 0.707}, // front center
	{0x8, 0.5, 0.5},     // LFE
	{0x10, 0.707, 0},    // back left
	{0x20, 0, 0.707},    // back right
	{0x40, 0.707, 0},    // front left of center
	{0x80, 0, 0.707},    // front right of center
	{0x100, 0.5, 0.5},   // back center
	{0x200, 0.707, 0},   // side left
	{0x400, 0, 0.707},   // side right
	{0x800, 0.5, 0.5},   // top center
	{0x1000, 0.707, 0},  // top front left
	{0x2000, 0.5, 0.5},  // top front center
	{0x4000, 0, 0.707},  // top front right
	{0x8000, 0.707, 0},  // top back left
	{0x10000, 0.5, 0.5}, // top back center
	{0x20000, 0, 0.707}, // top back right
}

// NewConverter builds a converter for frames of the given sample kind and
// channel count. mask is the channel mask, or 0 if unknown.
func NewConverter(kind SampleKind, channels int, mask uint32) *Converter {
	c := &Converter{kind: kind, channels: channels, matrix: make([][2]float32, channels), rng: 0x9E3779B9}
	switch {
	case channels == 1:
		c.matrix[0] = [2]float32{1, 1}
	case mask != 0 && bits.OnesCount32(mask) == channels:
		ch := 0
		for _, s := range speakerMix {
			if mask&s.bit != 0 {
				c.matrix[ch] = [2]float32{s.l, s.r}
				ch++
			}
		}
	default:
		// Unknown layout: the first two channels are left and right by
		// convention; anything else is dropped.
		c.matrix[0] = [2]float32{1, 0}
		if channels > 1 {
			c.matrix[1] = [2]float32{0, 1}
		}
	}
	return c
}

// FrameBytes is the size of one input frame.
func (c *Converter) FrameBytes() int { return c.kind.Bytes() * c.channels }

// Convert appends frames from src to dst as stereo s16 and reports whether
// every sample was silent. src holds whole frames; nil src means digital
// silence of the given length.
func (c *Converter) Convert(dst []int16, src []byte, frames int) ([]int16, bool) {
	silent := true
	size := c.kind.Bytes()
	for f := range frames {
		var l, r float32
		if src != nil {
			for ch := range c.channels {
				v := c.sample(src[(f*c.channels+ch)*size:])
				m := c.matrix[ch]
				l += v * m[0]
				r += v * m[1]
			}
		}
		if abs(l) >= silenceLevel || abs(r) >= silenceLevel {
			silent = false
		}
		dst = append(dst, c.quantize(l), c.quantize(r))
	}
	return dst, silent
}

func (c *Converter) sample(b []byte) float32 {
	switch c.kind {
	case Float32:
		return math.Float32frombits(binary.LittleEndian.Uint32(b))
	case Int16:
		return float32(int16(binary.LittleEndian.Uint16(b))) / (1 << 15)
	case Int24:
		return float32(int32(uint32(b[0])<<8|uint32(b[1])<<16|uint32(b[2])<<24)) / (1 << 31)
	default:
		return float32(int32(binary.LittleEndian.Uint32(b))) / (1 << 31)
	}
}

// quantize converts to s16 with triangular dither. Exact zeros stay zero so
// digital silence is sent as silence, not as dither noise.
func (c *Converter) quantize(v float32) int16 {
	if v == 0 {
		return 0
	}
	x := float64(v)*32767 + c.uniform() - c.uniform()
	return int16(math.Round(min(max(x, -32768), 32767)))
}

// uniform returns a value in [0, 1) from a xorshift generator.
func (c *Converter) uniform() float64 {
	c.rng ^= c.rng << 13
	c.rng ^= c.rng >> 17
	c.rng ^= c.rng << 5
	return float64(c.rng) / (1 << 32)
}

func abs(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}
