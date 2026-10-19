# Outloud

Use a Mac on your network as a wireless speaker for a Windows PC.

Outloud sends whatever your PC plays (games, videos, music, calls) to a Mac
on the same network, which plays it through its speakers or whatever it has
plugged in. Latency is around a tenth of a second, the two computers'
clocks are kept in step so the sound never drifts or stutters over time, and
neither side needs admin rights, a driver or an account.

It works the other way too: with its own switch, the PC can play what the
Mac plays, so you can hear the Mac on the PC's headphones. Either direction,
or both at once.

```
 Windows PC                                        Mac
 ┌──────────────────────────────┐                  ┌──────────────────────────────┐
 │ apps ─▶ playback device      │                  │ outloud-receiver             │
 │            │ loopback capture│   your network   │  jitter buffer (~80 ms)      │
 │ Outloud (tray app) ──────────┼─────────────────▶│  clock-drift correction      │
 │  48 kHz 16-bit stereo        │   found by       │  CoreAudio ─▶ speakers       │
 └──────────────────────────────┘   Bonjour        └──────────────────────────────┘
```

**Requirements:** an up-to-date Windows 10 or 11 on x64, and macOS 12 or
later on Apple silicon or Intel, on the same local network. Hearing the Mac
on the PC needs macOS 14.2 or later.

## Install

**1. On the PC**, open PowerShell and run:

```powershell
irm https://github.com/thomasreichmann/outloud/releases/latest/download/install-windows.ps1 | iex
```

It installs to `%LOCALAPPDATA%\Programs\Outloud`, adds Outloud to the Start
menu and to *Installed apps*, and starts it. A speaker icon appears in the
notification area (maybe under the **^** overflow arrow).

Prefer not to pipe a script? Download `Outloud-<version>-windows-x64.zip` from
the [latest release](https://github.com/thomasreichmann/outloud/releases/latest),
unzip it and run `install.cmd`.

**2. On the Mac**, install the receiver. The easiest way: in the tray menu
choose **Set up a Mac…**. It shows a command to paste into Terminal on the
Mac, which downloads the receiver straight from your PC. Or, in Terminal:

```sh
curl -fsSL https://github.com/thomasreichmann/outloud/releases/latest/download/install-mac.sh | sh
```

Either way the receiver starts right away and at every login. Allow it when
macOS asks whether Outloud may accept incoming connections or use the local
network, and allow Outloud when Windows asks about network access (private
networks are enough). The first time the PC listens to the Mac, macOS also
asks whether Outloud may record the Mac's audio; allow that to hear the Mac
on the PC.

**3. Pick the Mac** under **Speakers** in the tray menu. If it is the only one
on the network, a fresh install picks it by itself.

The binaries are not code-signed. Both one-line installers download with
tools that do not mark files as coming from the internet, so SmartScreen and
Gatekeeper do not interfere; the zip's installer removes that mark itself.

## Using it

| tray menu | |
|---|---|
| **Speakers** | the receivers on the network; tick one to stream to it |
| **Connect by address…** | for a Mac that is not listed: use the address its receiver prints, like `Alexs-MacBook-Pro.local` |
| **Stop streaming** | |
| **Hear on this PC** | tick a Mac to play its sound on this PC; tick it again to stop. Independent of streaming |
| **Latency** | how much audio the Mac buffers. *Low* (40 ms) keeps video in sync on a good network; *Safe* (200 ms) survives weak Wi-Fi |
| **Capture from** | the default playback device (follows changes), or a fixed one |
| **Follow Windows volume** | apply the PC's volume and mute on the Mac (off by default) |
| **Start with Windows** | |
| **Set up a Mac…** | serve the receiver installer to a Mac on the network |

The icon is dim while not connected, solid when connected, and shows blue
sound waves while sound is flowing. Nothing is sent while nothing plays, and
after 10 s of silence the Mac lets go of its audio device so it can sleep. If
the network hiccups and the sound drops out, the Mac buffers a little more
for a while, then creeps back to the latency you chose.

### Hearing the Mac on the PC

Under **Hear on this PC**, tick the Mac. Everything the Mac plays then also
plays on the PC's default output, with the same **Latency**. The Mac keeps
playing it too; turn the Mac down to hear it only on the PC (the Mac's volume
does not change what the PC gets). While a PC listens, macOS may show its
recording indicator in the menu bar.

Both directions can be on at once without the sound going round in circles:
the Mac never sends back what the PC sends it, and the PC leaves the Mac's
sound out of what it sends to the Mac. Leaving it out needs Windows 11
(strictly, Windows build 20348 or later); on older Windows, with both
directions on, the Mac hears itself again a moment later.

### Not hearing everything twice

Outloud sends what a playback device plays, and that device keeps playing it
too. To hear the sound only on the Mac, either use a PC output with nothing
attached, or, better, make the Mac a proper Windows output:

1. Install the free [VB-CABLE](https://vb-audio.com/Cable/) virtual audio device.
2. In Windows sound settings, rename its *CABLE Input* output to *MacBook*.
3. In the tray menu, set **Capture from** to it, and tick **Follow Windows volume**.

Now picking *MacBook* as the Windows output plays on the Mac, picking your
headphones plays on the headphones only, and the volume keys control
whichever is selected.

### Command line

`outloud-send.exe`, next to the tray app, does the same from a terminal, plus
some diagnostics:

```
outloud-send list                         receivers on the network
outloud-send devices                      playback devices
outloud-send meter                        captured levels and the device's real clock rate
outloud-send -to "Kitchen Mac" -v         stream, printing the Mac's buffer statistics
outloud-send -to 192.168.1.20 -tone       send a test beep instead of capturing
outloud-send listen -to "Kitchen Mac"     play the Mac's sound on this PC
outloud-send -to "Kitchen Mac" -hear      both directions at once
```

On the Mac, `outloud-receiver` runs in the foreground when started without
`install`, which is handy for watching what happens:

```sh
~/Library/Application\ Support/Outloud/outloud-receiver -v
~/Library/Application\ Support/Outloud/outloud-receiver install -name Kitchen -device-buffer 80ms
```

## Troubleshooting

- **The Mac is not listed.** Both computers must be on the same network, and
  the Mac must be awake. If macOS keeps the receiver from announcing itself,
  use **Connect by address…** with the address from the Mac's log.
- **Crackling.** Choose a higher **Latency**. If the crackle is steady rather
  than occasional, the Mac's own output buffer is too small: reinstall the
  receiver with `-device-buffer 80ms`.
- **No sound.** Run `outloud-send meter` while something plays, to check that
  capture works. Check the Mac's volume and output device. With **Follow
  Windows volume** on, a muted PC means a muted Mac.
- **Too quiet.** With **Follow Windows volume** on, the Mac also applies the
  PC's volume. Turn the PC up, or the option off.
- **It stops when the Mac sleeps.** A sleeping Mac cannot play. Keep it on
  power with the lid open, or prevent sleep in System Settings.
- **The Mac is missing under Hear on this PC.** Update the Mac's receiver
  (step 2 above): only receivers from 0.2.0 on, on macOS 14.2 or later, can
  share the Mac's sound.
- **Hearing the Mac stays silent.** macOS may not let Outloud record the
  Mac's audio: in System Settings ▸ Privacy & Security ▸ Screen & System
  Audio Recording, allow Outloud under *System Audio Recording Only*. The
  Mac's log says so too. macOS may ask again after the receiver is updated.

Logs: `%LOCALAPPDATA%\Outloud\Outloud.log` (tray menu ▸ **Open log**) and
`~/Library/Logs/Outloud.log` on the Mac.

## Uninstall

- **PC:** Settings ▸ Apps ▸ Installed apps ▸ Outloud ▸ Uninstall. This also
  removes its settings and logs.
- **Mac:** `~/Library/Application\ Support/Outloud/outloud-receiver uninstall`.
  This removes the receiver (`Outloud.app` in that folder) and its login
  item; the log stays.

## Limitations

- Audio is sent unencrypted, and the receiver plays for any sender on the
  network, and sends the Mac's sound to any PC that asks. Use it on networks
  you trust.
- One sender at a time: a second PC takes over from the first. Likewise one
  PC at a time can hear the Mac.
- The binaries are not code-signed, so macOS may ask again for permission to
  record the Mac's audio after the receiver is updated.

## How it works

The PC captures a playback device with WASAPI loopback capture, through
hand-written COM bindings, and sends 16-bit stereo PCM over TCP. The Mac keeps
a target amount of audio buffered. A PI controller watches the time-averaged
buffer level and resamples by a tiny, continuously adjusted ratio (4-point
Hermite interpolation), so the two sound cards' clocks never drift apart.
Receivers advertise themselves through the Mac's mDNSResponder, and the PC
finds them through Windows' DNS-SD API.

To hear the Mac, the PC opens a second connection and asks the receiver to
listen. The receiver captures the Mac's output with a Core Audio process tap
that leaves out its own playback, and streams it back with the same messages;
the PC plays it through the same jitter buffer. While it does, the PC
captures with process loopback, which leaves out Outloud's own playback. The
receiver installs as an app bundle, because macOS asks for permission to
record audio on behalf of apps.

| path | |
|---|---|
| `cmd/outloud` | Windows tray app |
| `cmd/outloud-send` | Windows command-line sender |
| `cmd/outloud-receiver` | Mac receiver, with `install` (LaunchAgent) |
| `internal/wasapi` | WASAPI loopback and process loopback capture |
| `internal/coreaudio` | the Mac's own sound, through a process tap |
| `internal/playout` | jitter buffer, drift-correcting resampler, adaptive latency |
| `internal/playback` | plays a playout on the default output, on either side |
| `internal/proto` | wire protocol |
| `internal/mdns` | discovery and advertising |
| `internal/sender`, `internal/receiver` | connection handling on each side |
| `internal/listener` | the PC's side of hearing the Mac |
| `internal/macsetup` | serves the receiver and `install.sh` to a Mac |

## Building

Go 1.27 or later; no C toolchain, everything cross-compiles with cgo off.

```sh
./build.sh            # → dist/Outloud
./build.sh release    # also → dist/release, as published on GitHub
go test ./...
```

Pushing a `v*` tag builds and publishes a release through GitHub Actions.

## License

[MIT](LICENSE)
