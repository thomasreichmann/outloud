//go:build windows

package main

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/thomasreichmann/outloud/internal/sender"
	"github.com/thomasreichmann/outloud/internal/update"
	"github.com/thomasreichmann/outloud/internal/version"
)

const (
	// updateDelay lets the network come up after login before the first
	// check for updates.
	updateDelay = 30 * time.Second
	updateEvery = 24 * time.Hour
	// updateRetry is how soon a check that failed is tried again.
	updateRetry = time.Hour
)

// macUpdate is a Mac that runs an older Outloud than it could, from and to
// being the versions.
type macUpdate struct {
	name, from, to string
}

// checkUpdates asks GitHub for the latest release a little after starting,
// and then once a day, while the user lets it.
func (a *app) checkUpdates() {
	wait := updateDelay
	for {
		select {
		case <-time.After(wait):
		case <-a.checkNow:
		}
		wait = updateEvery
		a.mu.Lock()
		on := !a.cfg.NoUpdateCheck
		a.mu.Unlock()
		if !on {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		r, err := update.Latest(ctx)
		cancel()
		if err != nil {
			a.log.Printf("checking for updates: %v", err)
			wait = updateRetry
			continue
		}
		a.mu.Lock()
		news := r.Version != a.latest.Version
		a.latest = r
		a.mu.Unlock()
		switch {
		case !news:
		case update.Older(version.Version, r.Version):
			a.log.Printf("Outloud %s is out: %s", r.Version, r.URL)
		default:
			a.log.Printf("up to date (the latest release is %s)", r.Version)
		}
		a.refreshMenu()
		a.refreshStatus()
	}
}

func (a *app) toggleUpdateCheck() {
	a.mu.Lock()
	turnOn := a.cfg.NoUpdateCheck
	a.latest = update.Release{}
	a.mu.Unlock()
	a.update(func(c *Config) { c.NoUpdateCheck = !turnOn })
	if turnOn {
		select {
		case a.checkNow <- struct{}{}:
		default:
		}
	}
	a.refreshStatus()
}

// showUpdateLocked offers the latest release if it is newer than this
// program. a.mu is held.
func (a *app) showUpdateLocked() {
	check(a.mUpdates, !a.cfg.NoUpdateCheck)
	if r := a.latest; update.Older(version.Version, r.Version) {
		a.mUpdate.SetTitle("Update to Outloud " + r.Version + "…")
		a.mUpdate.Show()
	} else {
		a.mUpdate.Hide()
	}
}

// showMacUpdateLocked offers to update the Mac in use if it runs an older
// Outloud than this PC or the latest release. a.mu is held.
func (a *app) showMacUpdateLocked() {
	var u macUpdate
	switch {
	case a.status.State == sender.Connected:
		u = macUpdate{name: a.status.Receiver, from: update.Software(a.status.Software)}
	case a.hearStatus.State == sender.Connected:
		u = macUpdate{name: a.hearStatus.Receiver, from: update.Software(a.hearStatus.Software)}
	}
	u.to = version.Version
	if update.Older(u.to, a.latest.Version) {
		u.to = a.latest.Version
	}
	if !update.Older(u.from, u.to) {
		u = macUpdate{}
	}
	if u == a.macUpdate {
		return
	}
	a.macUpdate = u
	if u.name == "" {
		a.mMacUpdate.Hide()
		return
	}
	a.mMacUpdate.SetTitle(u.name + " runs Outloud " + u.from + ": update it…")
	a.mMacUpdate.Show()
}

// updatePC installs the latest release, once the user agrees, with that
// release's installer, which replaces and restarts this program.
func (a *app) updatePC() {
	a.mu.Lock()
	r := a.latest
	a.mu.Unlock()
	if !update.Older(version.Version, r.Version) {
		return
	}
	if !askYesNo("Update Outloud", fmt.Sprintf("Outloud %s is out; this PC has %s.\nWhat is new: %s\n\n"+
		"Install it now? A window shows the installer at work. Outloud restarts, so the sound pauses for a moment.",
		r.Version, version.Version, r.URL)) {
		return
	}
	// The installer stops this program, so it runs on its own, in a console
	// of its own, which stays open if something goes wrong.
	script := fmt.Sprintf("try { irm '%s' | iex } catch { Write-Host $_ -ForegroundColor Red; Read-Host 'Press Enter to close' }",
		update.WindowsInstaller(r.Version))
	cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script)
	if err := cmd.Start(); err != nil {
		a.log.Printf("starting the installer: %v", err)
		messageBox("Update Outloud", "Could not start the installer: "+err.Error(), mbWarning)
		return
	}
	_ = cmd.Process.Release()
	a.log.Printf("updating to Outloud %s", r.Version)
}

// updateMac tells the user how to update the Mac in use.
func (a *app) updateMac() {
	a.mu.Lock()
	u := a.macUpdate
	a.mu.Unlock()
	if u.name == "" {
		return
	}
	msg := fmt.Sprintf("%s runs Outloud %s, and %s is out.\n\n"+
		"On the Mac, open Terminal (in Applications > Utilities) and run:\n\n%s\n\n", u.name, u.from, u.to, update.MacCommand)
	if setClipboard(update.MacCommand) == nil {
		msg += "The command is on this PC's clipboard.\n\n"
	}
	msg += "It installs the latest release. Or use Set up a Mac… in this menu, which installs this PC's version."
	messageBox("Update "+u.name, msg, mbInfo)
}
