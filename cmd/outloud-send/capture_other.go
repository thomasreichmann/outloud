//go:build !windows

package main

import (
	"context"
	"errors"

	"github.com/thomasreichmann/outloud/internal/source"
)

// Off Windows there is nothing to capture; only -tone works, for testing.
type noCapture struct{}

func (noCapture) Run(context.Context, func(source.Event)) error {
	return errors.New("capturing is only implemented on Windows; use -tone")
}

func newCapture(string, bool) source.Source { return noCapture{} }

func devices() error { return errors.New("only implemented on Windows") }
