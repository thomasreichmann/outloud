//go:build windows

// Command Outloud is the Windows tray app: it streams whatever this PC plays
// to a Mac running outloud-receiver, so the Mac works as a wireless speaker,
// and, independently, can play the Mac's own sound on this PC.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"

	"github.com/thomasreichmann/outloud/internal/listener"
	"github.com/thomasreichmann/outloud/internal/macsetup"
	"github.com/thomasreichmann/outloud/internal/mdns"
	"github.com/thomasreichmann/outloud/internal/pairing"
	"github.com/thomasreichmann/outloud/internal/playback"
	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/proto"
	"github.com/thomasreichmann/outloud/internal/sender"
	"github.com/thomasreichmann/outloud/internal/update"
	"github.com/thomasreichmann/outloud/internal/version"
	"github.com/thomasreichmann/outloud/internal/wasapi"
)

const (
	maxSpeakers  = 8
	maxDevices   = 12
	browseEvery  = 10 * time.Second
	devicesEvery = 15 * time.Second
	setupFor     = 30 * time.Minute

	outputRate = 48000
	// outputBuffer is the WASAPI buffer for the Mac's sound.
	outputBuffer = 20 * time.Millisecond
	// sleepAfter is how long the output stays idle before it is released.
	sleepAfter = 10 * time.Second
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
	// id is this PC's key, and peers the Macs paired with it.
	id    *pairing.Identity
	peers *pairing.Store

	// pairMu is held while pairing; lastPair is how the last one went.
	pairMu   sync.Mutex
	lastPair pairResult

	mu          sync.Mutex
	quitting    bool           // the tray icon is gone
	pairingWith string         // the Mac being paired with, for the status
	paired      []pairing.Peer // what the paired items currently show
	cfg         Config
	firstRun    bool
	found       []mdns.Service
	speakers    []speaker // what the speaker items currently show
	devices     []wasapi.Device
	status      sender.Status
	light       bool
	icons       icons
	shown       []byte // the icon currently set
	setupCmd    string
	setupStop   context.CancelFunc

	hearable   []speaker // what the hear items currently show
	hearStatus listener.Status

	// latest is the latest release, once GitHub has said, and macUpdate the
	// Mac that the Mac's update item is about. checkNow asks for a check.
	latest    update.Release
	macUpdate macUpdate
	checkNow  chan struct{}

	restartMu sync.Mutex
	running   *run
	hearing   *run
	// The Mac's sound plays through out, opened on first use: a process
	// gets one output.
	playout *playout.Playout
	out     *playback.Device

	mUpdate     *systray.MenuItem
	mMacUpdate  *systray.MenuItem
	mStatus     *systray.MenuItem
	mHearStatus *systray.MenuItem
	mSpeakers   []*systray.MenuItem
	mSearching  *systray.MenuItem
	mStop       *systray.MenuItem
	mHear       []*systray.MenuItem
	mNoHear     *systray.MenuItem
	mLatency    []*systray.MenuItem
	mDefaultDev *systray.MenuItem
	mDevices    []*systray.MenuItem
	mVolume     *systray.MenuItem
	mAutostart  *systray.MenuItem
	mUpdates    *systray.MenuItem
	mPaired     []*systray.MenuItem
	mNoPaired   *systray.MenuItem
}

func main() {
	if !firstInstance() {
		messageBox("Outloud", "Outloud is already running. Its speaker icon is in the notification area of the taskbar.", mbInfo)
		return
	}
	a := &app{checkNow: make(chan struct{}, 1)}
	a.log, a.logPath = openLog()
	// With no window, the tray would count as background work, and Windows
	// would slow it down now and then, holding up the sound.
	if err := wasapi.NoThrottling(); err != nil {
		a.log.Print(err)
	}
	a.hostname, _ = os.Hostname()
	var err error
	if a.id, err = pairing.LoadIdentity(dataDir("APPDATA")); err != nil {
		a.log.Printf("loading this PC's key: %v", err)
		messageBox("Outloud", "Outloud cannot start: loading this PC's key failed.\n\n"+err.Error(), mbWarning)
		return
	}
	a.peers = pairing.OpenStore(dataDir("APPDATA"))
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
	a.mUpdate = systray.AddMenuItem("", "Download and install the new release")
	a.mUpdate.Hide()
	onClick(a.mUpdate, a.updatePC)
	a.mMacUpdate = systray.AddMenuItem("", "How to update the Mac")
	a.mMacUpdate.Hide()
	onClick(a.mMacUpdate, a.updateMac)
	// The status items are enabled while another PC uses the Mac; clicking
	// one then takes the Mac over.
	a.mStatus = systray.AddMenuItem("Starting…", "")
	a.mStatus.Disable()
	onClick(a.mStatus, a.takeOver)
	a.mHearStatus = systray.AddMenuItem("", "")
	a.mHearStatus.Disable()
	a.mHearStatus.Hide()
	onClick(a.mHearStatus, a.takeOverHearing)
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

	hearHeader := systray.AddMenuItem("Hear on this PC", "")
	hearHeader.Disable()
	for i := range maxSpeakers {
		mi := systray.AddMenuItemCheckbox("", "Play this Mac's own sound on this PC", false)
		mi.Hide()
		a.mHear = append(a.mHear, mi)
		onClick(mi, func() { a.toggleHear(i) })
	}
	a.mNoHear = systray.AddMenuItem("No Mac on the network shares its sound", "")
	a.mNoHear.Disable()
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
	a.mUpdates = systray.AddMenuItemCheckbox("Check for updates", "Ask GitHub once a day whether a new release is out", false)
	onClick(a.mUpdates, a.toggleUpdateCheck)
	systray.AddSeparator()

	pairedMenu := systray.AddMenuItem("Paired Macs", "The Macs this PC can use")
	for i := range maxSpeakers {
		mi := pairedMenu.AddSubMenuItem("", "It has to pair again to be used")
		mi.Hide()
		a.mPaired = append(a.mPaired, mi)
		onClick(mi, func() { a.forgetMac(i) })
	}
	a.mNoPaired = pairedMenu.AddSubMenuItem("None yet: choose a Mac above to pair", "")
	a.mNoPaired.Disable()
	setup := systray.AddMenuItem("Set up a Mac…", "Install the receiver on a Mac on this network, and pair with it")
	onClick(setup, a.setupMac)
	logItem := systray.AddMenuItem("Open log", "")
	onClick(logItem, func() { openFile(a.logPath) })
	quit := systray.AddMenuItem("Quit", "")
	onClick(quit, systray.Quit)

	a.refreshDevices()
	a.refreshMenu()
	go a.discover()
	go a.watch()
	go a.checkUpdates()
	a.restart(false)
	a.restartHear(false)
}

func (a *app) exit() {
	// The icon is gone by now, and stopping each direction below reports a
	// last status: refreshing the menu or icon from here on only fails.
	a.mu.Lock()
	a.quitting = true
	a.mu.Unlock()
	a.restartMu.Lock()
	for _, r := range []*run{a.running, a.hearing} {
		if r != nil {
			r.cancel()
			<-r.done
		}
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
// config. takeOver says the user has just chosen the speaker, so the sender
// may take it from another PC; otherwise it waits until the speaker is free.
func (a *app) restart(takeOver bool) {
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
		// While a Mac's sound plays here, it must not be sent back.
		Source:        &wasapi.Loopback{Device: cfg.Device, ExcludeSelf: a.hearingOn},
		Target:        sender.Target{Name: cfg.Receiver, Addr: cfg.Address},
		Name:          a.hostname,
		Identity:      a.id,
		Peers:         a.peers,
		Latency:       time.Duration(cfg.LatencyMs) * time.Millisecond,
		ForwardVolume: cfg.ForwardVolume,
		TakeOver:      takeOver,
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

// restartHear stops playing a Mac's sound, if it is, and starts again for
// the current config. takeOver is as for restart.
func (a *app) restartHear(takeOver bool) {
	a.restartMu.Lock()
	defer a.restartMu.Unlock()
	if a.hearing != nil {
		a.hearing.cancel()
		<-a.hearing.done
		a.hearing = nil
	}
	a.mu.Lock()
	cfg := a.cfg
	a.hearStatus = listener.Status{State: sender.Connecting}
	a.mu.Unlock()
	if !cfg.hearing() {
		a.refreshStatus()
		return
	}
	p, err := a.output()
	if err != nil {
		a.log.Printf("opening the audio output: %v", err)
		a.mu.Lock()
		a.hearStatus = listener.Status{State: sender.Disconnected, Err: err}
		a.mu.Unlock()
		a.refreshStatus()
		return
	}
	a.refreshStatus()

	l := &listener.Listener{
		Target:   sender.Target{Name: cfg.Hear, Addr: cfg.HearAddress},
		Name:     a.hostname,
		Identity: a.id,
		Peers:    a.peers,
		Playout:  p,
		Latency:  time.Duration(cfg.LatencyMs) * time.Millisecond,
		Log:      a.log,
		TakeOver: takeOver,
		OnStatus: a.onHearStatus,
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{cancel: cancel, done: make(chan struct{})}
	a.hearing = r
	a.log.Printf("hearing %q, latency %d ms", cfg.Hear, cfg.LatencyMs)
	go func() {
		defer close(r.done)
		l.Run(ctx)
	}()
}

func (a *app) hearingOn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.hearing()
}

// output opens the audio output on first use. Called with restartMu held.
func (a *app) output() (*playout.Playout, error) {
	if a.playout != nil {
		return a.playout, nil
	}
	var out *playback.Device
	p := playout.New(playout.Config{OutputRate: outputRate, Target: 80 * time.Millisecond, OnWake: func() { out.Wake() }})
	out, err := playback.Open(p, p, outputRate, outputBuffer)
	if err != nil {
		return nil, err
	}
	a.playout, a.out = p, out
	go a.rest(p, out)
	return p, nil
}

// rest releases the output while nothing plays, and logs dropouts.
func (a *app) rest(p *playout.Playout, out *playback.Device) {
	idleSince := time.Now()
	var prev playout.Stats
	for range time.Tick(time.Second) {
		st := p.Stats()
		if st.Underruns > prev.Underruns {
			a.log.Printf("the Mac's sound ran dry; buffering %v for now", st.Target)
		}
		if st.State != playout.Idle {
			idleSince = time.Now()
			out.Wake()
		} else if time.Since(idleSince) >= sleepAfter {
			out.Sleep()
		}
		prev = st
	}
}

func targetName(c Config) string {
	if c.Receiver != "" {
		return fmt.Sprintf("%q", c.Receiver)
	}
	return c.Address
}

// update changes the config, saves it and restarts each direction whose
// settings changed.
func (a *app) update(f func(*Config)) { a.apply(f, false) }

// choose is update for when the user chooses a Mac: a direction that
// restarts may take the Mac from another PC.
func (a *app) choose(f func(*Config)) { a.apply(f, true) }

func (a *app) apply(f func(*Config), takeOver bool) {
	a.mu.Lock()
	before := a.cfg
	f(&a.cfg)
	after := a.cfg
	if after != before {
		a.saveLocked()
	}
	a.mu.Unlock()
	a.refreshMenu()
	if after.sendPart() != before.sendPart() {
		a.restart(takeOver)
	}
	if after.hearPart() != before.hearPart() {
		a.restartHear(takeOver)
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
	// Choosing the current speaker again retries at once, taking it from
	// another PC that uses it.
	again := s.name == a.cfg.Receiver && s.addr == a.cfg.Address && a.status.State != sender.Connected
	a.mu.Unlock()
	if again {
		a.restart(true)
		return
	}
	a.choose(func(c *Config) { c.Receiver, c.Address = s.name, s.addr })
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

// takeOver takes the speaker from the PC that uses it, if one does.
func (a *app) takeOver() {
	a.mu.Lock()
	by := sender.InUseBy(a.status.Err)
	a.mu.Unlock()
	if by != "" {
		a.log.Printf("taking the speaker over from %s", by)
		a.restart(true)
	}
}

// takeOverHearing takes the Mac's sound from the PC that hears it, if one
// does.
func (a *app) takeOverHearing() {
	a.mu.Lock()
	by := sender.InUseBy(a.hearStatus.Err)
	a.mu.Unlock()
	if by != "" {
		a.log.Printf("taking the Mac's sound over from %s", by)
		a.restartHear(true)
	}
}

func (a *app) stopStreaming() {
	a.update(func(c *Config) { c.Receiver, c.Address = "", "" })
}

// toggleHear starts or stops playing a Mac's sound on this PC.
func (a *app) toggleHear(i int) {
	a.mu.Lock()
	if i >= len(a.hearable) {
		a.mu.Unlock()
		return
	}
	s := a.hearable[i]
	a.mu.Unlock()
	a.choose(func(c *Config) {
		if strings.EqualFold(c.Hear, s.name) {
			c.Hear, c.HearAddress = "", ""
		} else {
			c.Hear, c.HearAddress = s.name, s.addr
		}
	})
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
	a.choose(func(c *Config) { c.Receiver, c.Address = "", addr })
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
		s := &macsetup.Server{
			Port: macsetup.DefaultPort, Identity: a.id, Peers: a.peers, Name: a.hostname, Logf: a.log.Printf,
			OnPaired: func(p pairing.Peer) {
				a.log.Printf("set up %q, which is now paired", p.Name)
				a.refreshMenu()
			},
		}
		c, err := s.Serve(ctx)
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

	msg := "On the Mac, open Terminal (in Applications > Utilities) and run:\n\n" + cmd + "\n\n"
	if setClipboard(cmd) == nil {
		msg += "The command is on this PC's clipboard. It is long, so it is easiest to send it " +
			"to the Mac, for example in a message to yourself, and paste it there.\n\n"
	}
	msg += "It downloads the receiver from this PC, checking that it really comes from this PC, " +
		"sets it to start at every login and pairs the Mac with this PC. " +
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

	if st.State == sender.Disconnected && errors.Is(st.Err, pairing.ErrNotPaired) {
		a.log.Printf("not connected: %v", st.Err)
		a.mu.Lock()
		name := a.cfg.Receiver
		a.mu.Unlock()
		go a.pairFor(toMac, name, st.Addr)
		a.refreshStatus()
		return
	}
	switch {
	case st.State != prev.State && st.State == sender.Connected:
		a.log.Printf("connected to %q at %s (%s)", st.Receiver, st.Addr, st.Software)
	case st.State != prev.State && sender.InUseBy(st.Err) != "":
		a.log.Printf("%s uses the speaker; waiting until it is free", sender.InUseBy(st.Err))
	case st.State != prev.State && st.State == sender.Disconnected && st.Err != nil:
		a.log.Printf("not connected: %v", st.Err)
	case st.Device != prev.Device && st.Device != "":
		a.log.Printf("capturing %q at %d Hz", st.Device, st.Rate)
	case st.Err != nil && st.State == sender.Connected && (prev.Err == nil || prev.Err.Error() != st.Err.Error()):
		a.log.Printf("capture: %v", st.Err)
	case st.Stats.Underruns > prev.Stats.Underruns:
		a.log.Printf("speaker's buffer ran dry; it now buffers %.0f ms", st.Stats.TargetMs)
	}
	if st.State == sender.Connected {
		if st.Stats.OutputError != prev.Stats.OutputError && st.Stats.OutputError != "" {
			a.log.Printf("%q cannot play: %s", st.Receiver, st.Stats.OutputError)
		}
		if st.Stats.Muted && !prev.Stats.Muted {
			a.log.Printf("%q is muted (%s)", st.Receiver, st.Stats.Output)
		}
	}
	a.refreshStatus()
}

func (a *app) onHearStatus(st listener.Status) {
	a.mu.Lock()
	prev := a.hearStatus
	a.hearStatus = st
	if st.State == sender.Connected && st.Addr != "" && a.cfg.Hear != "" && a.cfg.HearAddress != st.Addr {
		a.cfg.HearAddress = st.Addr
		a.saveLocked()
	}
	a.mu.Unlock()

	if st.State == sender.Disconnected && errors.Is(st.Err, pairing.ErrNotPaired) {
		a.log.Printf("not hearing: %v", st.Err)
		a.mu.Lock()
		name := a.cfg.Hear
		a.mu.Unlock()
		go a.pairFor(fromMac, name, st.Addr)
		a.refreshStatus()
		return
	}
	switch {
	case st.State != prev.State && st.State == sender.Connected:
		a.log.Printf("hearing %q at %s (%s)", st.Receiver, st.Addr, st.Software)
	case st.State != prev.State && sender.InUseBy(st.Err) != "":
		a.log.Printf("%s hears the Mac; waiting until it is free", sender.InUseBy(st.Err))
	case st.State != prev.State && st.State == sender.Disconnected && st.Err != nil:
		a.log.Printf("not hearing: %v", st.Err)
	}
	switch {
	case st.Capture == prev.Capture:
	case st.Capture.Error != "":
		a.log.Printf("%q cannot share its sound: %s", st.Receiver, st.Capture.Error)
	case prev.Capture.Error != "" && st.State == sender.Connected:
		a.log.Printf("%q shares its sound again", st.Receiver)
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
	if a.quitting {
		return
	}
	st, cfg := a.status, a.cfg
	name := cfg.Receiver
	if st.Receiver != "" {
		name = st.Receiver
	} else if name == "" {
		name = cfg.Address
	}

	// problem describes a connection that failed for a reason the user can
	// act on.
	problem := func(err error, name string) string {
		switch {
		case errors.Is(err, pairing.ErrNotPaired) && a.pairingWith != "":
			return "Pairing with " + name + ": compare the codes"
		case errors.Is(err, pairing.ErrNotPaired):
			return "Not paired with " + name
		case errors.Is(err, sender.ErrOutdated):
			return name + " runs an older Outloud: set it up again"
		case errors.Is(err, sender.ErrNewer):
			return name + " runs a newer Outloud: update this PC"
		}
		return ""
	}

	text, icon := "", a.icons.off
	inUse := cfg.streaming() && sender.InUseBy(st.Err) != ""
	switch {
	case !cfg.streaming():
		text = "Not streaming: choose a speaker"
	case inUse:
		text = sender.InUseBy(st.Err) + " is using " + name + ": click to take it over"
	case st.State == sender.Disconnected && problem(st.Err, name) != "":
		text = problem(st.Err, name)
	case st.State == sender.Connected && st.Err != nil:
		text, icon = "Connected to "+name+", but "+st.Err.Error(), a.icons.connected
	case st.State == sender.Connected && st.Stats.OutputError != "":
		text, icon = name+" cannot play: "+st.Stats.OutputError, a.icons.connected
	case st.State == sender.Connected && st.Stats.Muted && st.Playing:
		text, icon = "Streaming to "+name+", but the Mac is muted", a.icons.playing
	case st.State == sender.Connected && st.Stats.Muted:
		text, icon = "Connected to "+name+", but the Mac is muted", a.icons.connected
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
	enable(a.mStatus, inUse)
	tip := "Outloud\n" + text
	if st.State == sender.Connected && st.Stats.Output != "" {
		tip += "\nPlays on " + st.Stats.Output
	}
	if st.State == sender.Connected && st.Stats.TargetMs > 0 {
		tip += fmt.Sprintf("\n%.0f ms buffer", st.Stats.TargetMs)
	}

	hearInUse := false
	if hs := a.hearStatus; cfg.hearing() {
		name := cfg.Hear
		if hs.Receiver != "" {
			name = hs.Receiver
		}
		hearInUse = sender.InUseBy(hs.Err) != ""
		switch {
		case hearInUse:
			text = sender.InUseBy(hs.Err) + " is hearing " + name + ": click to hear it here instead"
		case hs.State == sender.Connected && hs.Capture.Problem == proto.CaptureNeedsPermission:
			text = name + ": click Allow on the Mac to let it share its sound"
		case hs.State == sender.Connected && hs.Capture.Problem == proto.CaptureDenied:
			text = name + " may not share its sound: see its Privacy & Security settings"
		case hs.State == sender.Connected && hs.Capture.Error != "":
			text = name + " cannot capture its sound: " + hs.Capture.Error
		case hs.State == sender.Connected:
			text = "Hearing " + name + " on this PC"
			if !cfg.streaming() || st.State != sender.Connected {
				icon = a.icons.connected
			}
		case hs.State == sender.Disconnected && problem(hs.Err, name) != "":
			text = problem(hs.Err, name)
		case hs.State == sender.Disconnected && hs.Err != nil:
			text = "Cannot reach " + name + " to hear it, retrying"
		default:
			text = "Looking for " + name + " to hear it…"
		}
		a.mHearStatus.SetTitle(text)
		a.mHearStatus.Show()
		tip += "\n" + text
	} else {
		a.mHearStatus.Hide()
	}
	enable(a.mHearStatus, hearInUse)
	if r := []rune(tip); len(r) > 127 {
		tip = string(r[:126]) + "…"
	}
	systray.SetTooltip(tip)
	a.showMacUpdateLocked()
	a.setIcon(icon)
}

// refreshMenu brings the speaker, device and option items in line with the
// current state.
func (a *app) refreshMenu() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.quitting {
		return
	}
	cfg := a.cfg

	// A receiver that speaks another protocol version works only once one
	// side is updated.
	label := func(s mdns.Service) string {
		switch v := proto.TXTVersion(s.TXT); {
		case v < proto.Version:
			return s.Instance + " (needs update)"
		case v > proto.Version:
			return s.Instance + " (update this PC)"
		}
		return s.Instance
	}
	var list []speaker
	listed := false
	for _, s := range a.found {
		list = append(list, speaker{label: label(s), name: s.Instance, addr: s.Addr()})
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

	// Only Macs that advertise it can share their sound; an older receiver
	// would take a listener for a sender.
	var hear []speaker
	listed = false
	for _, s := range a.found {
		if slices.Contains(s.TXT, proto.CaptureTXT) {
			hear = append(hear, speaker{label: label(s), name: s.Instance, addr: s.Addr()})
			listed = listed || strings.EqualFold(s.Instance, cfg.Hear)
		}
	}
	if cfg.Hear != "" && !listed {
		hear = append(hear, speaker{label: cfg.Hear + " (not found)", name: cfg.Hear, addr: cfg.HearAddress})
	}
	hear = hear[:min(len(hear), maxSpeakers)]
	a.hearable = hear
	for i, mi := range a.mHear {
		if i >= len(hear) {
			mi.Hide()
			continue
		}
		mi.SetTitle(hear[i].label)
		check(mi, strings.EqualFold(hear[i].name, cfg.Hear))
		mi.Show()
	}
	if len(hear) == 0 {
		a.mNoHear.Show()
	} else {
		a.mNoHear.Hide()
	}

	paired, err := a.peers.List()
	if err != nil {
		a.log.Printf("reading the paired Macs: %v", err)
	}
	a.paired = paired[:min(len(paired), maxSpeakers)]
	for i, mi := range a.mPaired {
		if i >= len(a.paired) {
			mi.Hide()
			continue
		}
		mi.SetTitle("Forget " + a.paired[i].Name + "…")
		mi.Show()
	}
	if len(a.paired) == 0 {
		a.mNoPaired.Show()
	} else {
		a.mNoPaired.Hide()
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
	a.showUpdateLocked()
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

// enable enables or disables mi, touching the menu only if that changes it,
// since the status refreshes every second.
func enable(mi *systray.MenuItem, on bool) {
	switch {
	case on && mi.Disabled():
		mi.Enable()
	case !on && !mi.Disabled():
		mi.Disable()
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
			changed = changed || svcs[i].Instance != a.found[i].Instance || svcs[i].Addr() != a.found[i].Addr() ||
				!slices.Equal(svcs[i].TXT, a.found[i].TXT)
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
			a.restart(false)
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
