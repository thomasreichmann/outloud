package main

import (
	"fmt"

	"github.com/thomasreichmann/outloud/internal/source"
	"github.com/thomasreichmann/outloud/internal/wasapi"
)

func newCapture(device string, native bool) source.Source {
	return &wasapi.Loopback{Device: device, NativeFormat: native}
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
