// Command outloud-send streams what this PC plays to an Outloud receiver.
// It is the command-line counterpart of the Outloud tray app.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/thomasreichmann/outloud/internal/listener"
	"github.com/thomasreichmann/outloud/internal/macsetup"
	"github.com/thomasreichmann/outloud/internal/mdns"
	"github.com/thomasreichmann/outloud/internal/pairing"
	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/proto"
	"github.com/thomasreichmann/outloud/internal/sender"
	"github.com/thomasreichmann/outloud/internal/source"
	"github.com/thomasreichmann/outloud/internal/version"
)

const outputRate = 48000

const usage = `Usage:
  outloud-send [flags]        stream this PC's audio to a receiver
  outloud-send listen [flags] play a receiver's own sound on this PC
  outloud-send pair [flags]   pair with a receiver, comparing a code
  outloud-send paired         list the receivers paired with this PC
  outloud-send forget NAME    unpair a receiver ("all" for every one)
  outloud-send list           find receivers on the network
  outloud-send devices        list playback devices
  outloud-send meter [flags]  show the level being captured, without sending
  outloud-send serve-mac      serve the receiver installer to a Mac, pairing with it
  outloud-send version        print the version

This PC's key and paired receivers are shared with the Outloud tray app.

Flags:
`

func main() {
	cmd := ""
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("outloud-send", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), usage); fs.PrintDefaults() }
	to := fs.String("to", "", "receiver `name or host[:port]` (default: the first one found)")
	device := fs.String("device", "", "capture this playback device (part of its name); default follows the Windows default")
	latency := fs.Duration("latency", 80*time.Millisecond, "audio the receiver buffers; lower is snappier, higher survives worse Wi-Fi")
	followVolume := fs.Bool("follow-volume", false, "apply the Windows volume and mute on the receiver")
	tone := fs.Bool("tone", false, "send a test beep instead of capturing")
	native := fs.Bool("native-format", false, "capture in the device's own format and convert here (diagnostics)")
	hear := fs.Bool("hear", false, "also play the receiver's own sound here, leaving it out of what is sent")
	excludeSelf := fs.Bool("exclude-self", false, "leave this program's own playback out of the capture (diagnostics; implied by -hear)")
	verbose := fs.Bool("v", false, "print the receiver's buffer statistics every 5 s")
	browseType := fs.String("browse-type", proto.ServiceType, "service type for list (diagnostics)")
	port := fs.Int("port", macsetup.DefaultPort, "port for serve-mac")
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var src source.Source = newCapture(*device, *native, *excludeSelf || *hear)
	if *tone {
		src = source.Tone{Freq: 440, Gain: 0.1}
	}
	keys := func() keys {
		id, err := pairing.LoadIdentity(dataDir())
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: loading this PC's key:", err)
			os.Exit(1)
		}
		return keys{id, pairing.OpenStore(dataDir())}
	}

	var err error
	switch cmd {
	case "":
		k := keys()
		if *hear {
			go func() {
				if err := listen(ctx, k, parseTarget(*to), *latency, *verbose); err != nil {
					logf("listening: %v", err)
				}
			}()
		}
		err = stream(ctx, k, src, parseTarget(*to), *latency, *followVolume, *verbose)
	case "listen":
		err = listen(ctx, keys(), parseTarget(*to), *latency, *verbose)
	case "pair":
		err = pair(ctx, keys(), parseTarget(*to))
	case "paired":
		err = listPaired(keys())
	case "forget":
		err = forget(keys(), fs.Args())
	case "list":
		err = list(ctx, *browseType)
	case "devices":
		err = devices()
	case "meter":
		err = meter(ctx, src)
	case "serve-mac":
		err = serveMac(ctx, keys(), *port)
	case "version":
		fmt.Println(version.String())
	default:
		fs.Usage()
		os.Exit(2)
	}
	if errors.Is(err, pairing.ErrNotPaired) {
		err = fmt.Errorf("%w; run \"outloud-send pair\" with the same -to to pair", err)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// keys are this PC's key and paired receivers, kept with the tray app's.
type keys struct {
	id    *pairing.Identity
	peers *pairing.Store
}

func dataDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "Outloud")
}

// parseTarget treats anything that looks like an address as one, and
// anything else as a receiver name.
func parseTarget(s string) sender.Target {
	host := s
	if h, _, err := net.SplitHostPort(s); err == nil {
		host = h
	}
	if s == "" || (net.ParseIP(host) == nil && !strings.Contains(host, ".")) {
		return sender.Target{Name: s}
	}
	return sender.Target{Addr: s}
}

func stream(ctx context.Context, k keys, src source.Source, target sender.Target, latency time.Duration, volume, verbose bool) error {
	name, _ := os.Hostname()
	var last sender.Status
	var lastStats time.Time
	s := &sender.Sender{
		Source:        src,
		Target:        target,
		Name:          name,
		Identity:      k.id,
		Peers:         k.peers,
		Latency:       latency,
		ForwardVolume: volume,
		OnStatus: func(st sender.Status) {
			switch {
			case st.State != last.State || st.Receiver != last.Receiver:
				switch st.State {
				case sender.Connecting:
					if target.Addr != "" {
						logf("connecting to %s ...", sender.WithPort(target.Addr))
					} else {
						logf("looking for %s ...", orDefault(target.Name, "a receiver"))
					}
				case sender.Connected:
					sw := st.Software
					if sw == "" {
						sw = "unknown version"
					}
					logf("connected to %q at %s (%s)", st.Receiver, st.Addr, sw)
				case sender.Disconnected:
					if st.Err != nil && !errors.Is(st.Err, pairing.ErrNotPaired) {
						logf("not connected: %v (retrying)", st.Err)
					}
				}
			case st.Device != last.Device || st.Rate != last.Rate:
				logf("capturing %q at %d Hz", st.Device, st.Rate)
			case st.Err != nil && (last.Err == nil || st.Err.Error() != last.Err.Error()):
				logf("capture: %v", st.Err)
			case st.Playing != last.Playing:
				if st.Playing {
					logf("streaming")
				} else {
					logf("idle")
				}
			case st.Stats.OutputError != last.Stats.OutputError && st.Stats.OutputError != "":
				logf("the receiver cannot play: %s", st.Stats.OutputError)
			case st.Stats.Muted != last.Stats.Muted && st.State == sender.Connected:
				if st.Stats.Muted {
					logf("the receiver's output (%s) is muted", st.Stats.Output)
				} else {
					logf("the receiver's output is no longer muted")
				}
			case verbose && st.State == sender.Connected && time.Since(lastStats) >= 5*time.Second:
				lastStats = time.Now()
				r := st.Stats
				logf("receiver %s: buffer %.1f/%.0f ms, clock correction %+.1f ppm, %d underruns, %d skips",
					r.State, r.BufferMs, r.TargetMs, r.DriftPPM, r.Underruns, r.Skips)
			}
			last = st
		},
	}
	return s.Run(ctx)
}

func listen(ctx context.Context, k keys, target sender.Target, latency time.Duration, verbose bool) error {
	var wake func()
	p := playout.New(playout.Config{OutputRate: outputRate, Target: latency, OnWake: func() { wake() }})
	wake, err := openOutput(p)
	if err != nil {
		return fmt.Errorf("opening audio output: %w", err)
	}
	go func() {
		var prev playout.Stats
		var lastLog time.Time
		for range time.Tick(time.Second) {
			st := p.Stats()
			switch {
			case st.State == playout.Playing && prev.State != playout.Playing:
				logf("playing (buffer %v)", st.Target)
			case st.State == playout.Idle && prev.State != playout.Idle:
				logf("idle")
			}
			if st.Underruns > prev.Underruns {
				logf("buffer ran dry; buffering %v for now", st.Target)
			}
			if verbose && st.State != playout.Idle && time.Since(lastLog) >= 5*time.Second {
				lastLog = time.Now()
				logf("buffer %.1f/%v, clock correction %+.1f ppm, %d underruns, %d skips",
					float64(st.Buffered)/float64(time.Millisecond), st.Target, st.DriftPPM, st.Underruns, st.Skips)
			}
			prev = st
		}
	}()

	name, _ := os.Hostname()
	var last listener.Status
	l := &listener.Listener{
		Target:   target,
		Name:     name,
		Identity: k.id,
		Peers:    k.peers,
		Playout:  p,
		Latency:  latency,
		Log:      log.New(os.Stdout, "", log.Ltime),
		OnStatus: func(st listener.Status) {
			if st.Capture != last.Capture && st.State == sender.Connected {
				if st.Capture.Error != "" {
					logf("%q cannot share its sound: %s", st.Receiver, st.Capture.Error)
				} else if last.Capture.Error != "" {
					logf("%q shares its sound again", st.Receiver)
				}
			}
			if st.State == last.State && st.Receiver == last.Receiver {
				last = st
				return
			}
			switch st.State {
			case sender.Connecting:
				logf("looking for %s ...", orDefault(target.Name+target.Addr, "a receiver"))
			case sender.Connected:
				logf("listening to %q at %s (%s)", st.Receiver, st.Addr, st.Software)
			case sender.Disconnected:
				if st.Err != nil && !errors.Is(st.Err, pairing.ErrNotPaired) {
					logf("not connected: %v (retrying)", st.Err)
				}
			}
			last = st
		},
	}
	return l.Run(ctx)
}

func list(ctx context.Context, service string) error {
	svcs, err := mdns.Browse(ctx, service, 2*time.Second)
	if err != nil {
		return err
	}
	if len(svcs) == 0 {
		fmt.Println("No receivers found. Is outloud-receiver running on the Mac, on this network?")
		return nil
	}
	for _, s := range svcs {
		fmt.Printf("%-32s %-22s %-28s %s\n", s.Instance, s.Addr(), strings.TrimSuffix(s.Host, "."), strings.Join(s.TXT, " "))
	}
	return nil
}

func meter(ctx context.Context, src source.Source) error {
	const window = 250 * time.Millisecond
	var sumSq float64
	var peak, n int
	last := time.Now()
	// The device's real sample rate, measured against the system clock.
	var rate, frames int
	var first, lastRate time.Time
	return src.Run(ctx, func(e source.Event) {
		switch e.Kind {
		case source.Format:
			logf("capturing %q at %d Hz", e.Device, e.Rate)
			rate, frames, first = e.Rate, 0, time.Time{}
		case source.Volume:
			logf("Windows volume: %.1f dB (gain %.3f)", 20*math.Log10(math.Max(float64(e.Gain), 1e-5)), e.Gain)
		case source.Idle:
			logf("idle")
			frames, first = 0, time.Time{}
		case source.Status:
			logf("capture: %v", e.Err)
		case source.Audio:
			if first.IsZero() {
				first, lastRate = time.Now(), time.Now()
			} else {
				frames += len(e.Samples) / 2
			}
			if el := time.Since(first); time.Since(lastRate) >= 5*time.Second && rate > 0 {
				lastRate = time.Now()
				got := float64(frames) / el.Seconds()
				logf("device clock: %.1f Hz over %.0f s (%+.0f ppm against the system clock)", got, el.Seconds(), (got/float64(rate)-1)*1e6)
			}
			for _, v := range e.Samples {
				sumSq += float64(v) * float64(v)
				peak = max(peak, abs(int(v)))
			}
			n += len(e.Samples)
			if time.Since(last) >= window && n > 0 {
				rms := 20 * math.Log10(math.Max(math.Sqrt(sumSq/float64(n))/32768, 1e-5))
				pk := 20 * math.Log10(math.Max(float64(peak)/32768, 1e-5))
				bar := strings.Repeat("#", max(0, int((rms+60)/2)))
				fmt.Printf("rms %6.1f dBFS  peak %6.1f dBFS  %s\n", rms, pk, bar)
				sumSq, peak, n, last = 0, 0, 0, time.Now()
			}
		}
	})
}

func serveMac(ctx context.Context, k keys, port int) error {
	name, _ := os.Hostname()
	s := &macsetup.Server{
		Port: port, Identity: k.id, Peers: k.peers, Name: name, Logf: logf,
		OnPaired: func(p pairing.Peer) { logf("paired with %q", p.Name) },
	}
	cmd, err := s.Serve(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\nOn the Mac, open Terminal and run:\n\n    %s\n\n"+
		"It installs the receiver from this PC and pairs the two.\n"+
		"If Windows asks whether to allow network access, allow it on private networks.\n"+
		"Press Ctrl+C here when the Mac is set up.\n\n", cmd)
	<-ctx.Done()
	return nil
}

func pair(ctx context.Context, k keys, target sender.Target) error {
	addr, err := sender.Resolve(ctx, target)
	if err != nil {
		return err
	}
	name, _ := os.Hostname()
	logf("pairing with the receiver at %s ...", addr)
	mac, err := sender.Pair(ctx, addr, name, k.id, k.peers, func(receiver, code string, cancel <-chan struct{}) bool {
		fmt.Printf("\n%q now asks whether to pair with this PC. Allow it there if it shows\n\n    %s\n\n"+
			"Does it show that code? [y/N] ", receiver, code)
		return askYes(cancel)
	})
	switch {
	case err == nil:
		logf("paired with %q", mac)
	case errors.Is(err, pairing.ErrCancelled):
		logf("not paired")
	case errors.Is(err, pairing.ErrDeclined):
		return fmt.Errorf("%q did not allow pairing", mac)
	default:
		return err
	}
	return nil
}

// askYes reads a yes or no from the terminal, giving up when cancel is
// closed.
func askYes(cancel <-chan struct{}) bool {
	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		answer <- strings.ToLower(strings.TrimSpace(line))
	}()
	select {
	case a := <-answer:
		return a == "y" || a == "yes"
	case <-cancel:
		fmt.Println()
		return false
	}
}

func listPaired(k keys) error {
	peers, err := k.peers.List()
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		fmt.Println("No receiver is paired with this PC.")
	}
	for _, p := range peers {
		fmt.Printf("%-32s paired %s, key %s\n", p.Name, p.Paired.Local().Format("2006-01-02 15:04"), pairing.Short(p.ID))
	}
	return nil
}

func forget(k keys, args []string) error {
	if len(args) != 1 {
		return errors.New(`say which receiver to forget, by name or key as "outloud-send paired" lists them, or "all"`)
	}
	gone, err := k.peers.Forget(args[0])
	if err != nil {
		return err
	}
	for _, p := range gone {
		fmt.Printf("Forgot %s.\n", p.Name)
	}
	if len(gone) == 0 && args[0] != "all" {
		return fmt.Errorf("no paired receiver is called %q", args[0])
	}
	return nil
}

func logf(format string, args ...any) {
	fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return fmt.Sprintf("%q", s)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
