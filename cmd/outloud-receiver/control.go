package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// askFor is how long a request to pair waits for an answer.
const askFor = 2 * time.Minute

// control lets "outloud-receiver allow" and "deny" answer a request to pair
// from a terminal, also over SSH, besides the question on the screen. They
// reach the running receiver through a socket in the data directory, which
// only this user can open.
type control struct {
	path string
	// askLocally asks on this computer's screen, or terminal elsewhere. An
	// error means it cannot ask; then only a terminal command answers.
	askLocally func(name, code string, cancel <-chan struct{}) (bool, error)

	mu  sync.Mutex
	cur *request
}

type request struct {
	name, code string
	answer     chan bool
}

// controlMsg is what a command sends: Op "pending" asks for the current
// request, and "answer" answers it. Allowing takes the code the PC shows.
type controlMsg struct {
	Op    string `json:"op"`
	Code  string `json:"code,omitempty"`
	Allow bool   `json:"allow,omitempty"`
}

type controlReply struct {
	Name  string `json:"name,omitempty"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

func socketPath() string { return filepath.Join(dataDir(), "receiver.sock") }

// listen serves commands until the returned function is called.
func (c *control) listen() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(c.path) // left by a receiver that did not exit cleanly
	l, err := net.Listen("unix", c.path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(c.path, 0o600)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go c.handle(conn)
		}
	}()
	return func() { l.Close() }, nil
}

func (c *control) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	var m controlMsg
	if err := json.NewDecoder(conn).Decode(&m); err != nil {
		return
	}
	_ = json.NewEncoder(conn).Encode(c.do(m))
}

func (c *control) do(m controlMsg) controlReply {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.cur
	if r == nil {
		return controlReply{Error: "no PC is asking to pair right now"}
	}
	switch m.Op {
	case "pending":
		return controlReply{Name: r.name, Code: r.code}
	case "answer":
		if m.Allow && digits(m.Code) != digits(r.code) {
			return controlReply{Name: r.name, Error: "that is not the code of " + r.name + "'s request. " +
				"If the PC shows another code than this Mac, something is in between: deny it"}
		}
		select {
		case r.answer <- m.Allow:
		default: // answered already
		}
		return controlReply{Name: r.name, Code: r.code}
	}
	return controlReply{Error: fmt.Sprintf("unknown request %q", m.Op)}
}

func digits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

// confirm is the receiver's Confirm: whichever answers first, the question
// on this computer or a terminal command, decides.
func (c *control) confirm(name, code string, cancel <-chan struct{}) bool {
	r := &request{name: name, code: code, answer: make(chan bool, 1)}
	c.mu.Lock()
	c.cur = r
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.cur == r {
			c.cur = nil
		}
		c.mu.Unlock()
	}()

	stop := make(chan struct{})
	defer close(stop)
	local := make(chan bool, 1)
	go func() {
		yes, err := c.askLocally(name, code, stop)
		if err != nil {
			log.Printf("cannot ask here whether to pair with %s (%v); answer with \"outloud-receiver allow\"", name, err)
			return
		}
		local <- yes
	}()
	timeout := time.NewTimer(askFor)
	defer timeout.Stop()
	select {
	case yes := <-local:
		return yes
	case yes := <-r.answer:
		if yes {
			log.Printf("allowed pairing with %s from a terminal", name)
		} else {
			log.Printf("denied pairing with %s from a terminal", name)
		}
		return yes
	case <-cancel:
	case <-timeout.C:
	}
	return false
}

// send sends m to the running receiver at path.
func send(path string, m controlMsg) (controlReply, error) {
	conn, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return controlReply{}, errors.New("cannot reach the receiver: is it running, and up to date?")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(m); err != nil {
		return controlReply{}, err
	}
	var r controlReply
	if err := json.NewDecoder(conn).Decode(&r); err != nil {
		return controlReply{}, err
	}
	if r.Error != "" {
		return r, errors.New(r.Error)
	}
	return r, nil
}

// allow shows the pending request to pair, or, given the code the PC
// shows, allows it.
func allow(args []string) error {
	if len(args) == 0 {
		r, err := send(socketPath(), controlMsg{Op: "pending"})
		if err != nil {
			return err
		}
		fmt.Printf("%s asks to pair, and this Mac shows the code %s.\n"+
			"If the PC shows the same code, allow it with:\n  outloud-receiver allow %s\n", r.Name, r.Code, digits(r.Code))
		return nil
	}
	r, err := send(socketPath(), controlMsg{Op: "answer", Code: strings.Join(args, ""), Allow: true})
	if err != nil {
		return err
	}
	fmt.Printf("Allowed %s. Pairing completes when the PC says yes too; \"outloud-receiver paired\" lists it then.\n", r.Name)
	return nil
}

// deny turns the pending request to pair away.
func deny([]string) error {
	r, err := send(socketPath(), controlMsg{Op: "answer"})
	if err != nil {
		return err
	}
	fmt.Printf("Turned %s away.\n", r.Name)
	return nil
}
