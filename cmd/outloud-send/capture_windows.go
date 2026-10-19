package main

import (
	"fmt"
	"time"

	"github.com/thomasreichmann/outloud/internal/playback"
	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/source"
	"github.com/thomasreichmann/outloud/internal/wasapi"
)

func newCapture(device string, native, excludeSelf bool) source.Source {
	return &wasapi.Loopback{Device: device, NativeFormat: native, ExcludeSelf: func() bool { return excludeSelf }}
}

func openOutput(p *playout.Playout) (func(), error) {
	d, err := playback.Open(p, p, outputRate, 20*time.Millisecond)
	if err != nil {
		return nil, err
	}
	return d.Wake, nil
}

func devices() error {
	devs, err := wasapi.Devices()
	if err != nil {
		return err
	}
	for _, d := range devs {
		mark := " "
		if d.Default {
			mark = "*"
		}
		fmt.Printf("%s %s\n", mark, d.Name)
	}
	fmt.Println("\n* = Windows default, which is captured unless -device says otherwise")
	return nil
}
