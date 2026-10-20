//go:build !darwin

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/source"
)

var (
	stdinOnce  sync.Once
	stdinLines = make(chan string)
)

// askLocally asks on the terminal whether to pair with the PC called name.
// One goroutine reads the terminal, so an unanswered question does not
// swallow the answer to the next.
func askLocally(name, code string, cancel <-chan struct{}) (bool, error) {
	stdinOnce.Do(func() {
		go func() {
			sc := bufio.NewScanner(os.Stdin)
			for sc.Scan() {
				stdinLines <- sc.Text()
			}
			close(stdinLines)
		}()
	})
	fmt.Printf("%s asks to pair, showing code %s. Allow it? [y/N] ", name, code)
	select {
	case line, ok := <-stdinLines:
		if !ok {
			return false, errors.New("no terminal to ask on")
		}
		line = strings.TrimSpace(line)
		return strings.EqualFold(line, "y") || strings.EqualFold(line, "yes"), nil
	case <-cancel:
		fmt.Println("(answered elsewhere)")
		return false, nil
	}
}

func startDevice(io.Reader, *playout.Playout, time.Duration) (output, error) {
	return nil, errors.New("playback is only implemented on macOS; use -null to test")
}

func systemCapture() (source.Source, string) { return nil, "" }

func systemOutput() (string, bool, error) { return "", false, nil }

func computerName() string { return hostName() }

func localHostName() string { return hostName() }

func install([]string) error { return errors.New("install is only implemented on macOS") }

func trimLog() {}

func uninstall() error { return errors.New("uninstall is only implemented on macOS") }
