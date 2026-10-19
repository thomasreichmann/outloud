//go:build windows

package main

import (
	"image"
	"image/color"

	"github.com/thomasreichmann/outloud/internal/icon"
)

// icons holds the tray icon for each state, as .ico files.
type icons struct {
	off       []byte // not streaming, or the speaker is unreachable
	connected []byte
	playing   []byte
}

// traySizes are the sizes Windows uses in the notification area at the
// common display scales.
var traySizes = []int{16, 20, 24, 32, 48}

func makeIcons(light bool) icons {
	fg := color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF} // dark taskbar
	if light {
		fg = color.NRGBA{0x20, 0x20, 0x20, 0xFF}
	}
	dim := fg
	dim.A = 0x70
	accent := color.NRGBA{0x2E, 0xA8, 0xF0, 0xFF}
	return icons{
		off:       icon.ICO(func(s int) *image.NRGBA { return icon.Speaker(s, dim, dim, false) }, traySizes...),
		connected: icon.ICO(func(s int) *image.NRGBA { return icon.Speaker(s, fg, fg, false) }, traySizes...),
		playing:   icon.ICO(func(s int) *image.NRGBA { return icon.Speaker(s, fg, accent, true) }, traySizes...),
	}
}
