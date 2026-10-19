//go:build !windows

package main

import (
	"context"
	"errors"
	"time"

	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/source"
)

// Off Windows there is nothing to capture; only -tone works, for testing.
type noCapture struct{}

func (noCapture) Run(context.Context, func(source.Event)) error {
	return errors.New("capturing is only implemented on Windows; use -tone")
}

func newCapture(string, bool, bool) source.Source { return noCapture{} }

func devices() error { return errors.New("only implemented on Windows") }

// openOutput consumes p in real time without a sound card, for testing.
func openOutput(p *playout.Playout) (func(), error) {
	go func() {
		const chunk = 240 // 5 ms
		buf := make([]byte, 8*chunk)
		start := time.Now()
		var done int
		for range time.Tick(5 * time.Millisecond) {
			for due := int(time.Since(start).Seconds() * outputRate); done+chunk <= due; done += chunk {
				p.Read(buf)
			}
		}
	}()
	return func() {}, nil
}
