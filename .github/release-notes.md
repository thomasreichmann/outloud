## What's new in 0.2.0

- **Hear the Mac on the PC.** Under **Hear on this PC** in the tray menu,
  tick the Mac to play its sound on the PC, independently of streaming the
  PC to the Mac. Needs macOS 14.2 or later, and macOS asks once for
  permission to record the Mac's audio.
- Both directions at once do not echo: each side leaves out what the other
  sends it (on the PC, this needs Windows 11).
- The Mac receiver now installs as `Outloud.app`. Update both the PC and the
  Mac with the commands below; older receivers do not appear under **Hear on
  this PC**.

## Install

**On the Windows PC**, in PowerShell:

```powershell
irm https://github.com/thomasreichmann/outloud/releases/latest/download/install-windows.ps1 | iex
```

**On the Mac**, use **Set up a Mac…** in the tray menu, or in Terminal:

```sh
curl -fsSL https://github.com/thomasreichmann/outloud/releases/latest/download/install-mac.sh | sh
```

The binaries are not code-signed. Both installers download with tools that do
not mark files as coming from the internet, so neither SmartScreen nor
Gatekeeper gets in the way. See the [README](https://github.com/thomasreichmann/outloud#readme)
for the zip download, uninstalling and troubleshooting.

| file | |
|---|---|
| `Outloud-*-windows-x64.zip` | the Windows app, command-line sender and Mac receivers; run `install.cmd` |
| `outloud-receiver-darwin-arm64` / `-amd64` | the Mac receiver for Apple silicon / Intel |
| `install-windows.ps1`, `install-mac.sh` | the one-line installers |
| `SHA256SUMS` | checksums |
