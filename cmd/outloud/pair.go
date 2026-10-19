//go:build windows

package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/thomasreichmann/outloud/internal/pairing"
	"github.com/thomasreichmann/outloud/internal/sender"
)

// direction is one of the two things this PC does with a Mac.
type direction int

const (
	toMac   direction = iota // streaming this PC's sound to it
	fromMac                  // hearing the Mac's sound here
)

// pairResult is how the last pairing went. Both directions can be turned
// away by the same Mac at once: the first asks, and the second uses the
// answer, once, so that a Mac that keeps refusing cannot cause a loop.
type pairResult struct {
	addr string
	at   time.Time
	err  error
}

// pairFor pairs with the Mac at addr, which turned dir away for not being
// paired, and then starts dir again. name is what the settings call the Mac
// ("" for one chosen by address). If they do not pair, dir stops using the
// Mac, so nothing keeps asking; choosing the Mac again asks again.
func (a *app) pairFor(dir direction, name, addr string) {
	a.pairMu.Lock()
	defer a.pairMu.Unlock()
	last := a.lastPair
	err := last.err
	if last.addr == addr && time.Since(last.at) < 30*time.Second {
		a.lastPair = pairResult{}
	} else {
		label := name
		if label == "" {
			label = addr
		}
		err = a.pairWith(label, addr)
		a.lastPair = pairResult{addr: addr, at: time.Now(), err: err}
	}

	a.mu.Lock()
	still := false
	switch dir {
	case toMac:
		still = a.cfg.Receiver == name && (name != "" || sender.WithPort(a.cfg.Address) == addr)
	case fromMac:
		still = a.cfg.Hear == name
	}
	a.mu.Unlock()
	switch {
	case !still:
		// The settings moved on while pairing.
	case err == nil && dir == toMac:
		a.restart()
	case err == nil:
		a.restartHear()
	case dir == toMac:
		a.update(func(c *Config) { c.Receiver, c.Address = "", "" })
	default:
		a.update(func(c *Config) { c.Hear, c.HearAddress = "", "" })
	}
}

// pairWith asks the Mac at addr to pair, showing the code here. label names
// the Mac in messages until it says its own name.
func (a *app) pairWith(label, addr string) error {
	a.setPairing(label)
	defer a.setPairing("")
	a.log.Printf("pairing with %s at %s", label, addr)
	ctx, cancel := context.WithTimeout(context.Background(), pairing.Timeout)
	defer cancel()
	mac, err := sender.Pair(ctx, addr, a.hostname, a.id, a.peers, func(mac, code string, cancel <-chan struct{}) bool {
		// Logged for whoever answers on the Mac from a terminal.
		a.log.Printf("pairing with %q: this PC shows the code %s", mac, code)
		return confirmCode(mac, code, cancel)
	})
	if mac == "" {
		mac = label
	}
	switch {
	case err == nil:
		a.log.Printf("paired with %q", mac)
		a.refreshMenu()
	case errors.Is(err, pairing.ErrCancelled):
		a.log.Printf("did not pair with %q: cancelled here", mac)
	case errors.Is(err, pairing.ErrDeclined):
		a.log.Printf("did not pair with %q: not allowed there", mac)
		messageBox("Outloud", mac+" did not allow pairing with this PC.\n\n"+
			"To try again, choose it in Outloud's menu and click Allow on the Mac.", mbWarning)
	default:
		a.log.Printf("pairing with %q: %v", mac, err)
		messageBox("Outloud", "Could not pair with "+mac+": "+err.Error(), mbWarning)
	}
	return err
}

func (a *app) setPairing(label string) {
	a.mu.Lock()
	a.pairingWith = label
	a.mu.Unlock()
	a.refreshStatus()
}

// confirmCode shows the code that both sides of a pairing show, and asks
// whether the Mac shows the same.
func confirmCode(mac, code string, cancel <-chan struct{}) bool {
	return askOKCancel("Pair with "+mac,
		mac+" is asking whether to pair with this PC, and shows a code.\n\n"+
			"If the code is\n\n        "+code+"\n\n"+
			"click Allow on the Mac and OK here. Once paired, this PC can play its sound "+
			"on the Mac and hear the Mac's sound.", cancel)
}

// forgetMac unpairs the i-th paired Mac and stops using it.
func (a *app) forgetMac(i int) {
	a.mu.Lock()
	if i >= len(a.paired) {
		a.mu.Unlock()
		return
	}
	p := a.paired[i]
	a.mu.Unlock()
	if !askYesNo("Forget "+p.Name, "Forget "+p.Name+"?\n\n"+
		"This PC stops using it until the two pair again. To also remove this PC from the Mac, "+
		"run \"~/Library/Application\\ Support/Outloud/outloud-receiver forget "+a.hostname+"\" there.") {
		return
	}
	if err := a.peers.Remove(p.ID); err != nil {
		a.log.Printf("forgetting %q: %v", p.Name, err)
		messageBox("Outloud", "Could not forget "+p.Name+": "+err.Error(), mbWarning)
		return
	}
	a.log.Printf("forgot %q", p.Name)
	a.update(func(c *Config) {
		if strings.EqualFold(c.Receiver, p.Name) || a.status.PeerID == p.ID {
			c.Receiver, c.Address = "", ""
		}
		if strings.EqualFold(c.Hear, p.Name) || a.hearStatus.PeerID == p.ID {
			c.Hear, c.HearAddress = "", ""
		}
	})
}
