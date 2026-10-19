// Package icon draws Outloud's speaker glyph and packs it into .ico files,
// for the tray icon and for the Windows executable.
package icon

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Speaker draws the glyph on a transparent background, with sound waves if
// waves is set. Each pixel is 4×4 supersampled for anti-aliasing.
func Speaker(size int, body, wave color.NRGBA, waves bool) *image.NRGBA {
	return render(size, func(x, y float64) (color.NRGBA, bool) {
		switch {
		case inBody(x, y):
			return body, true
		case waves && inWaves(x, y):
			return wave, true
		}
		return color.NRGBA{}, false
	})
}

// App draws the application icon: a white speaker with waves on a blue
// rounded square, legible on light and dark backgrounds alike.
func App(size int) *image.NRGBA {
	bg := color.NRGBA{0x1F, 0x7A, 0xD6, 0xFF}
	white := color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF}
	return render(size, func(x, y float64) (color.NRGBA, bool) {
		if !inRoundedSquare(x, y, 0.04, 0.2) {
			return color.NRGBA{}, false
		}
		// The glyph, shrunk to leave a margin inside the square.
		gx, gy := (x-0.5)/0.72+0.5, (y-0.5)/0.72+0.5
		if inBody(gx, gy) || inWaves(gx, gy) {
			return white, true
		}
		return bg, true
	})
}

// render supersamples shape, which maps unit coordinates to a color.
func render(size int, shape func(x, y float64) (color.NRGBA, bool)) *image.NRGBA {
	const ss = 4
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for py := range size {
		for px := range size {
			var r, g, b, a float64
			for sy := range ss {
				for sx := range ss {
					x := (float64(px) + (float64(sx)+0.5)/ss) / float64(size)
					y := (float64(py) + (float64(sy)+0.5)/ss) / float64(size)
					if c, ok := shape(x, y); ok {
						w := float64(c.A) / 255
						r += float64(c.R) * w
						g += float64(c.G) * w
						b += float64(c.B) * w
						a += w
					}
				}
			}
			if a > 0 {
				img.SetNRGBA(px, py, color.NRGBA{
					R: uint8(r / a), G: uint8(g / a), B: uint8(b / a),
					A: uint8(math.Round(a / (ss * ss) * 255)),
				})
			}
		}
	}
	return img
}

// inBody reports whether (x, y), in unit coordinates, is inside the box and
// cone of the speaker.
func inBody(x, y float64) bool {
	if x >= 0.08 && x <= 0.30 && y >= 0.35 && y <= 0.65 {
		return true
	}
	// The cone widens from the box's height at x=0.30 to full height at
	// x=0.56.
	if x < 0.30 || x > 0.56 {
		return false
	}
	half := 0.15 + (x-0.30)/(0.56-0.30)*(0.40-0.15)
	return math.Abs(y-0.5) <= half
}

// inWaves reports whether (x, y) is on one of two arcs right of the cone.
func inWaves(x, y float64) bool {
	dx, dy := x-0.52, y-0.5
	if dx <= 0 || math.Abs(math.Atan2(dy, dx)) > 0.9 {
		return false
	}
	r := math.Hypot(dx, dy)
	return math.Abs(r-0.19) < 0.05 || math.Abs(r-0.36) < 0.05
}

func inRoundedSquare(x, y, inset, radius float64) bool {
	lo, hi := inset+radius, 1-inset-radius
	dx := math.Max(0, math.Max(lo-x, x-hi))
	dy := math.Max(0, math.Max(lo-y, y-hi))
	return x >= inset && x <= 1-inset && y >= inset && y <= 1-inset && math.Hypot(dx, dy) <= radius
}

// ICO packs PNG renderings at the given sizes into an .ico file.
func ICO(draw func(size int) *image.NRGBA, sizes ...int) []byte {
	var pngs [][]byte
	for _, s := range sizes {
		var b bytes.Buffer
		_ = png.Encode(&b, draw(s))
		pngs = append(pngs, b.Bytes())
	}
	var out bytes.Buffer
	le := func(v any) { _ = binary.Write(&out, binary.LittleEndian, v) }
	le([3]uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		dim := uint8(s) // 0 means 256
		le([4]uint8{dim, dim, 0, 0})
		le([2]uint16{1, 32})
		le([2]uint32{uint32(len(pngs[i])), uint32(offset)})
		offset += len(pngs[i])
	}
	for _, p := range pngs {
		out.Write(p)
	}
	return out.Bytes()
}
