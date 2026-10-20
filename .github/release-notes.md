## Outloud 1.0

The first stable release. From now on, every 1.x release works with every
other: any 1.x PC with any 1.x Mac. Each one also reads the settings, keys
and pairings an older one left behind.
[COMPATIBILITY.md](https://github.com/thomasreichmann/outloud/blob/v1.0.0/COMPATIBILITY.md)
says exactly what that covers.

What Outloud does:
- **A Mac becomes the PC's speaker.** It plays whatever the PC plays, over
  the local network, about a tenth of a second behind, and the two clocks
  are kept in step so the sound never drifts.
- **The PC can hear the Mac,** in either direction or both at once (macOS
  14.2 or later).
- **Paired and encrypted.** A Mac only plays for, and only shares its sound
  with, PCs it is paired with. Pairing means comparing a six-digit code once,
  or nothing at all when the PC sets the Mac up.
- **Little to look after.** Setting up a Mac is one Terminal command from
  the tray menu. The tray offers new releases, and the Mac keeps its
  permission to record across updates. When two PCs share a Mac, the one
  that took it last keeps it, and the other waits its turn.

Outloud 1.0 is 0.5.0 with this promise; nothing else changed.

**Updating:** from 0.4 or 0.5 nothing is asked, so the tray's **Update to
Outloud 1.0.0…** and the commands below are all it takes. From 0.3, macOS
asks once more whether Outloud may record the Mac's sound. From 0.2, update
both the PC and the Mac: the first time the PC picks the Mac, the two pair.

## Install

**On the Windows PC**, in PowerShell:

```powershell
irm https://github.com/thomasreichmann/outloud/releases/latest/download/install-windows.ps1 | iex
```

**On the Mac**, use **Set up a Mac…** in the tray menu, or in Terminal:

```sh
curl -fsSL https://github.com/thomasreichmann/outloud/releases/latest/download/install-mac.sh | sh
```

The Windows programs are not code-signed, and the Mac receiver is signed with
Outloud's own certificate rather than one from Apple. Both installers
download with tools that do not mark files as coming from the internet, so
neither SmartScreen nor Gatekeeper gets in the way. See the [README](https://github.com/thomasreichmann/outloud#readme)
for the zip download, uninstalling and troubleshooting.

| file | |
|---|---|
| `Outloud-*-windows-x64.zip` | the Windows app, command-line sender and Mac receivers; run `install.cmd` |
| `outloud-receiver-darwin-arm64.tar.gz` / `-amd64.tar.gz` | the Mac receiver, `Outloud.app`, for Apple silicon / Intel |
| `install-windows.ps1`, `install-mac.sh` | the one-line installers |
| `SHA256SUMS` | checksums |
