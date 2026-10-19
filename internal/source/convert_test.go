package source

import (
	"encoding/binary"
	"math"
	"testing"
)

func float32Frames(vals ...float32) []byte {
	b := make([]byte, 4*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(v))
	}
	return b
}

func near(t *testing.T, what string, got int16, want float64) {
	t.Helper()
	if math.Abs(float64(got)-want) > 1.5 {
		t.Errorf("%s = %d, want %.0f±1", what, got, want)
	}
}

func TestStereoFloat(t *testing.T) {
	c := NewConverter(Float32, 2, 0x3)
	out, silent := c.Convert(nil, float32Frames(0.5, -0.25, 1.5, -1.5), 2)
	if silent || len(out) != 4 {
		t.Fatalf("silent=%v len=%d", silent, len(out))
	}
	near(t, "left", out[0], 0.5*32767)
	near(t, "right", out[1], -0.25*32767)
	if out[2] != 32767 || out[3] != -32768 {
		t.Errorf("overs not clipped: %d %d", out[2], out[3])
	}
}

func TestMonoIsDuplicated(t *testing.T) {
	c := NewConverter(Float32, 1, 0)
	out, _ := c.Convert(nil, float32Frames(0.5), 1)
	near(t, "left", out[0], 0.5*32767)
	near(t, "right", out[1], 0.5*32767)
}

func TestSurroundDownmix(t *testing.T) {
	// 5.1 with only the center channel sounding.
	c := NewConverter(Float32, 6, 0x3F)
	out, _ := c.Convert(nil, float32Frames(0, 0, 0.5, 0, 0, 0), 1)
	near(t, "left", out[0], 0.5*0.707*32767)
	near(t, "right", out[1], 0.5*0.707*32767)
}

func TestInt24(t *testing.T) {
	c := NewConverter(Int24, 2, 0)
	// 0x400000 is half scale, 0xC00000 is minus half scale.
	out, _ := c.Convert(nil, []byte{0x00, 0x00, 0x40, 0x00, 0x00, 0xC0}, 1)
	near(t, "left", out[0], 16384)
	near(t, "right", out[1], -16384)
}

func TestSilence(t *testing.T) {
	c := NewConverter(Float32, 2, 0x3)
	out, silent := c.Convert(nil, float32Frames(1e-7, -1e-7), 1)
	if !silent {
		t.Error("near-zero input not reported silent")
	}
	out, silent = c.Convert(out[:0], nil, 4)
	if !silent || len(out) != 8 {
		t.Fatalf("nil src: silent=%v len=%d", silent, len(out))
	}
	for _, v := range out {
		if v != 0 {
			t.Fatalf("digital silence came out as %v", out)
		}
	}
}
