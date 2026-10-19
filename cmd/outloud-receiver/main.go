// Command outloud-receiver plays audio streamed from a Windows PC running
// Outloud, turning this computer into a network speaker. It is meant for
// macOS; elsewhere it only runs with -null, for testing.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/thomasreichmann/outloud/internal/mdns"
	"github.com/thomasreichmann/outloud/internal/playout"
	"github.com/thomasreichmann/outloud/internal/proto"
	"github.com/thomasreichmann/outloud/internal/receiver"
	"github.com/thomasreichmann/outloud/internal/version"
)

const (
	outputRate = 48000
	// sleepAfter is how long the receiver stays idle before releasing the
	// audio device, which lets the Mac sleep.
	sleepAfter = 10 * time.Second
)

// output is where the playout's samples go.
type output interface {
	// Wake makes sure the device is running; audio is arriving.
	Wake()
	// Sleep releases the device if the playout is still idle.
	Sleep()
	Err() error
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "install" || os.Args[1] == "uninstall") {
		var err error
		if os.Args[1] == "install" {
			err = install(os.Args[2:])
		} else {
			err = uninstall()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	fs := flag.NewFlagSet("outloud-receiver", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage: outloud-receiver [flags]
       outloud-receiver install [flags]   start at login (macOS LaunchAgent)
       outloud-receiver uninstall

Plays audio streamed from a Windows PC running Outloud. %s.

Flags:
`, version.String())
		fs.PrintDefaults()
	}
	name := fs.String("name", "", "name shown to senders (default: this computer's name)")
	port := fs.Int("port", proto.DefaultPort, "TCP port to listen on")
	latency := fs.Duration("latency", 80*time.Millisecond, "audio to buffer when the sender does not choose")
	deviceBuffer := fs.Duration("device-buffer", 40*time.Millisecond, "CoreAudio buffer; raise it if playback crackles")
	record := fs.String("record", "", "also write everything played to this WAV file")
	null := fs.Bool("null", false, "do not open an audio device; consume audio in real time (testing)")
	verbose := fs.Bool("v", false, "log buffer statistics every 10 s while playing")
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Println(version.String())
		return
	}
	if *name == "" {
		*name = computerName()
	}

	var out output
	p := playout.New(playout.Config{
		OutputRate: outputRate,
		Target:     *latency,
		OnWake:     func() { out.Wake() },
	})
	var src io.Reader = p
	var rec *wavWriter
	if *record != "" {
		var err error
		if rec, err = newWAVWriter(*record, outputRate); err != nil {
			log.Fatal(err)
		}
		src = io.TeeReader(p, rec)
	}
	if *null {
		out = startNull(src)
	} else {
		var err error
		if out, err = startDevice(src, p, *deviceBuffer); err != nil {
			log.Fatalf("opening audio output: %v", err)
		}
	}

	l, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatal(err)
	}
	stopAdvertising, err := mdns.Advertise(*name, proto.ServiceType, *port, []string{"v=1"})
	if err != nil {
		log.Printf("not advertised on the network (%v); senders need this computer's address", err)
		stopAdvertising = func() {}
	}
	srv := &receiver.Server{Name: *name, Playout: p, Latency: *latency, Log: log.Default()}
	go func() {
		if err := srv.Serve(l); err != nil {
			log.Fatal(err)
		}
	}()
	log.Printf("%s: %q is ready on port %d", version.String(), *name, *port)
	log.Printf("if it does not show up on the PC, connect by address: %s", strings.Join(addresses(*port), " or "))
	go monitor(p, out, *verbose)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	stopAdvertising()
	if rec != nil {
		if err := rec.Close(); err != nil {
			log.Print(err)
		}
	}
	log.Print("stopped")
}

// monitor logs notable playback events and lets the device rest while idle.
func monitor(p *playout.Playout, out output, verbose bool) {
	var (
		prev      playout.Stats
		idleSince = time.Now()
		lastLog   time.Time
		lastErr   error
	)
	for range time.Tick(time.Second) {
		st := p.Stats()
		switch {
		case st.State == playout.Playing && prev.State != playout.Playing && st.Underruns == prev.Underruns:
			log.Printf("playing (buffer %v)", st.Target)
		case st.State == playout.Idle && prev.State != playout.Idle:
			log.Print("stopped")
		}
		if st.Underruns > prev.Underruns {
			log.Printf("buffer ran dry; buffering %v for now (asked for %v)", st.Target, st.Requested)
		}
		if st.Skips > prev.Skips {
			log.Printf("network stalled; skipped ahead to keep latency at %v", st.Target)
		}
		if verbose && st.State != playout.Idle && time.Since(lastLog) >= 10*time.Second {
			lastLog = time.Now()
			log.Printf("buffer %.1f/%v, clock correction %+.1f ppm, %d underruns, %d skips",
				float64(st.Buffered)/float64(time.Millisecond), st.Target, st.DriftPPM, st.Underruns, st.Skips)
		}
		if err := out.Err(); err != nil && err != lastErr {
			log.Printf("audio output: %v", err)
			lastErr = err
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

// nullOutput consumes audio at real-time pace without a sound card.
type nullOutput struct{}

func startNull(src io.Reader) output {
	go func() {
		const chunk = 240 // 5 ms
		buf := make([]byte, 8*chunk)
		start := time.Now()
		var done int
		for range time.Tick(5 * time.Millisecond) {
			for due := int(time.Since(start).Seconds() * outputRate); done+chunk <= due; done += chunk {
				src.Read(buf)
			}
		}
	}()
	return nullOutput{}
}

func (nullOutput) Wake()      {}
func (nullOutput) Sleep()     {}
func (nullOutput) Err() error { return nil }

// addresses lists ways a sender can reach this computer: its .local name and
// its address on the local network.
func addresses(port int) []string {
	var out []string
	suffix := ""
	if port != proto.DefaultPort {
		suffix = fmt.Sprintf(":%d", port)
	}
	if h := localHostName(); h != "" {
		out = append(out, h+".local"+suffix)
	}
	if c, err := net.Dial("udp4", "192.0.2.1:9"); err == nil { // sends nothing
		out = append(out, c.LocalAddr().(*net.UDPAddr).IP.String()+suffix)
		c.Close()
	}
	return out
}

func hostName() string {
	h, err := os.Hostname()
	if err != nil {
		return "Outloud speaker"
	}
	return strings.TrimSuffix(h, ".local")
}
