package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// startControl serves commands for a receiver whose on-screen question
// waits until it is cancelled, or fails if broken.
func startControl(t *testing.T, broken bool) (*control, chan bool) {
	t.Helper()
	cancelled := make(chan bool, 1)
	c := &control{
		path: filepath.Join(t.TempDir(), "r.sock"),
		askLocally: func(name, code string, cancel <-chan struct{}) (bool, error) {
			if broken {
				return false, errors.New("no screen")
			}
			<-cancel
			cancelled <- true
			return false, nil
		},
	}
	stop, err := c.listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return c, cancelled
}

// waitPending waits until the receiver reports a request.
func waitPending(t *testing.T, c *control) controlReply {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if r, err := send(c.path, controlMsg{Op: "pending"}); err == nil {
			return r
		}
	}
	t.Fatal("no request appeared")
	return controlReply{}
}

func TestAllowFromTerminal(t *testing.T) {
	c, cancelled := startControl(t, false)
	if _, err := send(c.path, controlMsg{Op: "pending"}); err == nil {
		t.Error("a request is pending before anyone asked")
	}
	result := make(chan bool, 1)
	go func() { result <- c.confirm("DESKTOP", "123 456", nil) }()
	if r := waitPending(t, c); r.Name != "DESKTOP" || r.Code != "123 456" {
		t.Errorf("pending %+v", r)
	}
	if _, err := send(c.path, controlMsg{Op: "answer", Code: "123457", Allow: true}); err == nil {
		t.Error("a wrong code was accepted")
	}
	if _, err := send(c.path, controlMsg{Op: "answer", Code: "123 456", Allow: true}); err != nil {
		t.Fatal(err)
	}
	if !<-result {
		t.Error("allowing from the terminal did not allow")
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Error("the question on screen stayed up")
	}
	if _, err := send(c.path, controlMsg{Op: "pending"}); err == nil {
		t.Error("the request is still pending after being answered")
	}
}

// TestDenyWithoutScreen checks that a receiver that cannot ask on screen
// waits for a terminal instead of declining at once.
func TestDenyWithoutScreen(t *testing.T) {
	c, _ := startControl(t, true)
	result := make(chan bool, 1)
	go func() { result <- c.confirm("DESKTOP", "123 456", nil) }()
	waitPending(t, c)
	if _, err := send(c.path, controlMsg{Op: "answer"}); err != nil {
		t.Fatal(err)
	}
	if <-result {
		t.Error("denying from the terminal allowed")
	}
}
