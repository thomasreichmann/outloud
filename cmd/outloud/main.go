//go:build windows

// Command Outloud is the Windows tray app: it streams whatever this PC plays
// to a Mac running outloud-receiver, so the Mac works as a wireless speaker.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"

	"github.com/thomasreichmann/outloud/internal/macsetup"
	"github.com/thomasreichmann/outloud/internal/mdns"
	"github.com/thomasreichmann/outloud/internal/proto"
	"github.com/thomasreichmann/outloud/internal/sender"
	"github.com/thomasreichmann/outloud/internal/version"
	"github.com/thomasreichmann/outloud/internal/wasapi"
)

const (
	maxSpeakers  = 8
	maxDevices   = 12
	browseEvery  = 10 * time.Second
	devicesEvery = 15 * time.Second
	setupFor     = 30 * time.Minute
)

var latencies = []struct {
	label string
	ms    int
}{
	{"Low: 40 ms (good network)", 40},
	{"Normal: 80 ms", 80},
	{"Safe: 200 ms (weak Wi-Fi)", 200},
}

// speaker is one entry in the speaker list.
type speaker struct {
	label string
	name  string // advertised name; empty for an address-only entry
	addr  string
}

type run struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type app struct {
	log      *log.Logger
	logPath  string
	hostname string

	mu        sync.Mutex
	cfg       Config
	firstRun  bool
	found     []mdns.Service
	speakers  []speaker // what the speaker items currently show
	devices   []wasapi.Device
	status    sender.Status
	light     bool
	icons     icons
	shown     []byte // the icon currently set
	setupCmd  string
	setupStop context.CancelFunc

	restartMu sync.Mutex
	running   *run

	mStatus     *systray.MenuItem
	mSpeakers   []*systray.MenuItem
	mSearching  *systray.MenuItem
	mStop       *systray.MenuItem
	mLatency    []*systray.MenuItem
	mDefaultDev *systray.MenuItem
	mDevices    []*systray.MenuItem
	mVolume     *systray.MenuItem
	mAutostart  *systray.MenuItem
}

func main() {
	if !firstInstance() {
		messageBox("Outloud", "Outloud is already running. Its speaker icon is in the notification area of the taskbar.", mbInfo)
		return
	}
	a := &app{}
	a.log, a.logPath = openLog()
	a.hostname, _ = os.Hostname()
	var existed bool
	a.cfg, existed = loadConfig()
	a.firstRun = !existed
	a.light = lightTaskbar()
	a.icons = makeIcons(a.light)
	a.log.Printf("%s started (%s)", version.String(), exePath())
	systray.Run(a.ready, a.exit)
}

func openLog() (*log.Logger, string) {
	dir := dataDir("LOCALAPPDATA")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "Outloud.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > 1<<20 {
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return log.New(os.Stderr, "", log.LstdFlags), path
	}
	// The tray library reports its errors through the standard logger,
	// which would otherwise go nowhere in a GUI app.
	log.SetOutput(f)
	return log.New(f, "", log.LstdFlags), path
}

func onClick(mi *systray.MenuItem, f func()) {
	go func() {
		for range mi.ClickedCh {
			f()
		}
	}()
}

func (a *app) ready() {
	a.setIcon(a.icons.off)
	systray.SetTitle("Outloud")
	systray.SetTooltip("Outloud")

	title := systray.AddMenuItem(version.String(), "")
	title.Disable()
	a.mStatus = systray.AddMenuItem("Starting…", "")
	a.mStatus.Disable()
	systray.AddSeparator()

	header := systray.AddMenuItem("Speakers", "")
	header.Disable()
	for i := range maxSpeakers {
		mi := systray.AddMenuItemCheckbox("", "Stream to this speaker", false)
		mi.Hide()
		a.mSpeakers = append(a.mSpeakers, mi)
		onClick(mi, func() { a.chooseSpeaker(i) })
	}
	a.mSearching = systray.AddMenuItem("Searching the network…", "")
	a.mSearching.Disable()
	byAddress := systray.AddMenuItem("Connect by address…", "For a Mac that does not show up in the list")
	onClick(byAddress, a.connectByAddress)
	a.mStop = systray.AddMenuItem("Stop streaming", "")
	onClick(a.mStop, a.stopStreaming)
	systray.AddSeparator()

	latency := systray.AddMenuItem("Latency", "How much audio the Mac buffers: less is snappier, more survives a weaker network")
	for _, l := range latencies {
		mi := latency.AddSubMenuItemCheckbox(l.label, "", false)
		a.mLatency = append(a.mLatency, mi)
		onClick(mi, func() { a.update(func(c *Config) { c.LatencyMs = l.ms }) })
	}
	capture := systray.AddMenuItem("Capture from", "Which playback device's sound to send")
	a.mDefaultDev = capture.AddSubMenuItemCheckbox("Default playback device", "Follows the Windows default", false)
	onClick(a.mDefaultDev, func() { a.update(func(c *Config) { c.Device = "" }) })
	for i := range maxDevices {
		mi := capture.AddSubMenuItemCheckbox("", "", false)
		mi.Hide()
		a.mDevices = append(a.mDevices, mi)
		onClick(mi, func() { a.chooseDevice(i) })
	}
	a.mVolume = systray.AddMenuItemCheckbox("Follow Windows volume", "Apply this PC's volume and mute on the Mac", false)
	onClick(a.mVolume, func() { a.update(func(c *Config) { c.ForwardVolume = !c.ForwardVolume }) })
	a.mAutostart = systray.AddMenuItemCheckbox("Start with Windows", "", autostartEnabled())
	onClick(a.mAutostart, a.toggleAutostart)
	systray.AddSeparator()

	setup := systray.AddMenuItem("Set up a Mac…", "Install the receiver on a Mac on this network")
	onClick(setup, a.setupMac)
	logItem := systray.AddMenuItem("Open log", "")
	onClick(logItem, func() { openFile(a.logPath) })
	quit := systray.AddMenuItem("Quit", "")
	onClick(quit, systray.Quit)

	a.refreshDevices()
	a.refreshMenu()
	go a.discover()
	go a.watch()
	a.restart()
}

func (a *app) exit() {
	a.restartMu.Lock()
	if a.running != nil {
		a.running.cancel()
		<-a.running.done
	}
	a.restartMu.Unlock()
	a.mu.Lock()
	if a.setupStop != nil {
		a.setupStop()
	}
	a.mu.Unlock()
	a.log.Print("quit")
}

// restart stops the current sender, if any, and starts one for the current
// config.
func (a *app) restart() {
	a.restartMu.Lock()
	defer a.restartMu.Unlock()
	if a.running != nil {
		a.running.cancel()
		<-a.running.done
		a.running = nil
	}
	a.mu.Lock()
	cfg := a.cfg
	a.status = sender.Status{State: sender.Connecting}
	a.mu.Unlock()
	a.refreshStatus()
	if !cfg.streaming() {
		a.log.Print("not streaming")
		return
	}

	s := &sender.Sender{
		Source:        &wasapi.Loopback{Device: cfg.Device},
		Target:        sender.Target{Name: cfg.Receiver, Addr: cfg.Address},
		Name:          a.hostname,
		Latency:       time.Duration(cfg.LatencyMs) * time.Millisecond,
		ForwardVolume: cfg.ForwardVolume,
		OnStatus:      a.onStatus,
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{cancel: cancel, done: make(chan struct{})}
	a.running = r
	a.log.Printf("streaming to %s, latency %d ms, device %q, follow volume %v",
		targetName(cfg), cfg.LatencyMs, cfg.Device, cfg.ForwardVolume)
	go func() {
		defer close(r.done)
		s.Run(ctx)
	}()
}

func targetName(c Config) string {
	if c.Receiver != "" {
		return fmt.Sprintf("%q", c.Receiver)
	}
	return c.Address
}

// update changes the config, saves it and restarts streaming if it changed.
func (a *app) update(f func(*Config)) {
	a.mu.Lock()
	before := a.cfg
	f(&a.cfg)
	changed := a.cfg != before
	if changed {
		a.saveLocked()
	}
	a.mu.Unlock()
	a.refreshMenu()
	if changed {
		a.restart()
	}
}

func (a *app) saveLocked() {
	if err := saveConfig(a.cfg); err != nil {
		a.log.Printf("saving settings: %v", err)
	}
}

func (a *app) chooseSpeaker(i int) {
	a.mu.Lock()
	if i >= len(a.speakers) {
		a.mu.Unlock()
		return
	}
	s := a.speakers[i]
	a.mu.Unlock()
	a.update(func(c *Config) { c.Receiver, c.Address = s.name, s.addr })
}

func (a *app) chooseDevice(i int) {
	a.mu.Lock()
	if i >= len(a.devices) {
		a.mu.Unlock()
		return
	}
	name := a.devices[i].Name
	a.mu.Unlock()
	a.update(func(c *Config) { c.Device = name })
}

func (a *app) stopStreaming() {
	a.update(func(c *Config) { c.Receiver, c.Address = "", "" })
}

func (a *app) connectByAddress() {
	a.mu.Lock()
	cur := a.cfg.Address
	a.mu.Unlock()
	addr, ok := inputBox("Outloud",
		"The Mac's address or host name, as shown by outloud-receiver.\nFor example 192.168.1.20 or Alexs-MacBook-Pro.local", cur)
	if !ok {
		return
	}
	a.update(func(c *Config) { c.Receiver, c.Address = "", addr })
}

func (a *app) toggleAutostart() {
	on := !autostartEnabled()
	if err := setAutostart(on); err != nil {
		a.log.Printf("start with Windows: %v", err)
	}
	if autostartEnabled() {
		a.mAutostart.Check()
	} else {
		a.mAutostart.Uncheck()
	}
}

func (a *app) setupMac() {
	a.mu.Lock()
	cmd := a.setupCmd
	if cmd == "" {
		ctx, cancel := context.WithTimeout(context.Background(), setupFor)
		c, err := macsetup.Serve(ctx, macsetup.DefaultPort, a.log.Printf)
		if err != nil {
			cancel()
			a.mu.Unlock()
			messageBox("Set up a Mac", "Could not start the installer: "+err.Error(), mbWarning)
			return
		}
		a.setupCmd, a.setupStop, cmd = c, cancel, c
		context.AfterFunc(ctx, func() {
			a.mu.Lock()
			a.setupCmd = ""
			a.mu.Unlock()
		})
	}
	a.mu.Unlock()

	msg := "On the Mac, open Terminal (in Applications > Utilities) and run:\n\n    " + cmd + "\n\n"
	if setClipboard(cmd) == nil {
		msg += "(The command is also on this PC's clipboard.)\n\n"
	}
	msg += "It downloads the receiver from this PC and sets it to start at every login. " +
		"The Mac then appears under Speakers in this menu.\n\n" +
		"If Windows asks whether Outloud may use the network, allow it on private networks. " +
		"The installer stays available for 30 minutes."
	messageBox("Set up a Mac", msg, mbInfo)
}

func (a *app) onStatus(st sender.Status) {
	a.mu.Lock()
	prev := a.status
	a.status = st
	// Remember where the speaker was, for when discovery fails.
	if st.State == sender.Connected && st.Addr != "" && a.cfg.Receiver != "" && a.cfg.Address != st.Addr {
		a.cfg.Address = st.Addr
		a.saveLocked()
	}
	a.mu.Unlock()

	switch {
	case st.State != prev.State && st.State == sender.Connected:
		a.log.Printf("connected to %q at %s (%s)", st.Receiver, st.Addr, st.Software)
	case st.State != prev.State && st.State == sender.Disconnected && st.Err != nil:
		a.log.Printf("not connected: %v", st.Err)
	case st.Device != prev.Device && st.Device != "":
		a.log.Printf("capturing %q at %d Hz", st.Device, st.Rate)
	case st.Err != nil && st.State == sender.Connected && (prev.Err == nil || prev.Err.Error() != st.Err.Error()):
		a.log.Printf("capture: %v", st.Err)
	case st.Stats.Underruns > prev.Stats.Underruns:
		a.log.Printf("speaker's buffer ran dry; it now buffers %.0f ms", st.Stats.TargetMs)
	}
	a.refreshStatus()
}

func (a *app) setIcon(b []byte) {
	if len(a.shown) == len(b) && string(a.shown) == string(b) {
		return
	}
	a.shown = b
	systray.SetIcon(b)
}

func (a *app) refreshStatus() {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, cfg := a.status, a.cfg
	name := cfg.Receiver
	if st.Receiver != "" {
		name = st.Receiver
	} else if name == "" {
		name = cfg.Address
	}

	text, icon := "", a.icons.off
	switch {
	case !cfg.streaming():
		text = "Not streaming: choose a speaker"
	case st.State == sender.Connected && st.Err != nil:
		text, icon = "Connected to "+name+", but "+st.Err.Error(), a.icons.connected
	case st.State == sender.Connected && st.Playing:
		text, icon = "Streaming to "+name, a.icons.playing
	case st.State == sender.Connected:
		text, icon = "Connected to "+name, a.icons.connected
	case st.State == sender.Disconnected && st.Err != nil:
		text = "Cannot reach " + name + ", retrying"
	default:
		text = "Looking for " + name + "…"
	}
	if r := []rune(text); len(r) > 80 {
		text = string(r[:79]) + "…"
	}
	a.mStatus.SetTitle(text)
	tip := "Outloud\n" + text
	if st.State == sender.Connected && st.Stats.TargetMs > 0 {
		tip += fmt.Sprintf("\n%.0f ms buffer", st.Stats.TargetMs)
	}
	if r := []rune(tip); len(r) > 127 {
		tip = string(r[:126]) + "…"
	}
	systray.SetTooltip(tip)
	a.setIcon(icon)
}

// refreshMenu brings the speaker, device and option items in line with the
// current state.
func (a *app) refreshMenu() {
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg := a.cfg

	var list []speaker
	listed := false
	for _, s := range a.found {
		list = append(list, speaker{label: s.Instance, name: s.Instance, addr: s.Addr()})
		listed = listed || strings.EqualFold(s.Instance, cfg.Receiver)
	}
	switch {
	case cfg.Receiver != "" && !listed:
		list = append(list, speaker{label: cfg.Receiver + " (not found)", name: cfg.Receiver, addr: cfg.Address})
	case cfg.Receiver == "" && cfg.Address != "":
		list = append(list, speaker{label: cfg.Address, addr: cfg.Address})
	}
	list = list[:min(len(list), maxSpeakers)]
	a.speakers = list
	for i, mi := range a.mSpeakers {
		if i >= len(list) {
			mi.Hide()
			continue
		}
		s := list[i]
		mi.SetTitle(s.label)
		chosen := (s.name != "" && strings.EqualFold(s.name, cfg.Receiver)) ||
			(s.name == "" && cfg.Receiver == "" && s.addr == cfg.Address)
		check(mi, chosen)
		mi.Show()
	}
	if len(list) == 0 {
		a.mSearching.Show()
	} else {
		a.mSearching.Hide()
	}
	if cfg.streaming() {
		a.mStop.Enable()
	} else {
		a.mStop.Disable()
	}

	for i, l := range latencies {
		check(a.mLatency[i], cfg.LatencyMs == l.ms)
	}

	defName := ""
	devs := slices.Clone(a.devices)
	if cfg.Device != "" && !containsDevice(devs, cfg.Device) {
		devs = append(devs, wasapi.Device{Name: cfg.Device})
	}
	for i, mi := range a.mDevices {
		if i >= len(devs) {
			mi.Hide()
			continue
		}
		d := devs[i]
		if d.Default {
			defName = d.Name
		}
		label := d.Name
		if d.ID == "" {
			label += " (not connected)"
		}
		mi.SetTitle(label)
		check(mi, cfg.Device == d.Name)
		mi.Show()
	}
	a.mDefaultDev.SetTitle("Default playback device")
	if defName != "" {
		a.mDefaultDev.SetTitle("Default playback device: " + defName)
	}
	check(a.mDefaultDev, cfg.Device == "")
	check(a.mVolume, cfg.ForwardVolume)
}

func containsDevice(devs []wasapi.Device, name string) bool {
	for _, d := range devs {
		if d.Name == name {
			return true
		}
	}
	return false
}

func check(mi *systray.MenuItem, on bool) {
	if on {
		mi.Check()
	} else {
		mi.Uncheck()
	}
}

// discover keeps the speaker list current.
func (a *app) discover() {
	for {
		svcs, err := mdns.Browse(context.Background(), proto.ServiceType, 1500*time.Millisecond)
		if err != nil {
			a.log.Printf("searching for speakers: %v", err)
		}
		a.mu.Lock()
		changed := len(svcs) != len(a.found)
		for i := range svcs {
			changed = changed || svcs[i].Instance != a.found[i].Instance || svcs[i].Addr() != a.found[i].Addr()
		}
		a.found = svcs
		// On first run, a single speaker on the network is surely the one.
		auto := a.firstRun && len(svcs) == 1 && !a.cfg.streaming()
		if auto {
			a.cfg.Receiver, a.cfg.Address = svcs[0].Instance, svcs[0].Addr()
			a.saveLocked()
		}
		if len(svcs) > 0 {
			a.firstRun = false
		}
		a.mu.Unlock()
		if changed {
			var names []string
			for _, s := range svcs {
				names = append(names, fmt.Sprintf("%q at %s", s.Instance, s.Addr()))
			}
			a.log.Printf("speakers on the network: %d %v", len(svcs), names)
			a.refreshMenu()
			a.log.Print("menu updated")
		}
		if auto {
			a.log.Printf("first run: streaming to the only speaker found, %q", svcs[0].Instance)
			a.refreshMenu()
			a.restart()
		}
		time.Sleep(browseEvery)
	}
}

// watch follows playback devices and the taskbar theme.
func (a *app) watch() {
	for range time.Tick(devicesEvery) {
		if a.refreshDevices() {
			a.refreshMenu()
		}
		if light := lightTaskbar(); light != a.light {
			a.mu.Lock()
			a.light, a.icons = light, makeIcons(light)
			a.mu.Unlock()
			a.refreshStatus()
		}
	}
}

// refreshDevices reloads the device list and reports whether it changed.
func (a *app) refreshDevices() bool {
	devs, err := wasapi.Devices()
	if err != nil {
		a.log.Printf("listing playback devices: %v", err)
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	changed := len(devs) != len(a.devices)
	for i := range devs {
		changed = changed || devs[i] != a.devices[i]
	}
	a.devices = devs
	return changed
}
