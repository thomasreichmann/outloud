## What's new in 0.3.1

- **The tray says what is wrong on the Mac.** While hearing the Mac, it says
  when macOS is asking on the Mac for permission to record ("click Allow on
  the Mac") or when that was refused. While streaming, it says when the Mac
  is muted or cannot play, and its tooltip names the Mac's output device.
- Hearing the Mac no longer reconnects over and over while macOS asks for
  that permission.
- 0.3.1 and 0.3.0 work together, so updating one side at a time is fine.

## What's new in 0.3.0

- **Pairing, and encryption.** A Mac now only plays sound from PCs it is
  paired with, and only they can hear it. Everything between them is
  encrypted. Before, anyone on the network could do either.
- **Pairing takes one look.** The first time you pick a Mac, the PC and the
  Mac show a six-digit code. If they match, click **Allow** on the Mac and
  **OK** on the PC. Not at the Mac? Run `outloud-receiver allow` with the
  PC's code in its Terminal, also over SSH.
- **Set up a Mac…** now pairs the Mac as it installs it, so it asks nothing.
  The command it shows checks that the receiver really comes from this PC.
- Forget a Mac under **Paired Macs** in the tray menu; on the Mac,
  `outloud-receiver paired` and `outloud-receiver forget` do the same.

**Coming from 0.2? Update both the PC and the Mac**, with the commands
below: 0.3 cannot talk to older versions. An older Mac shows as "(needs update)" in the tray
menu; an older PC reports "peer speaks protocol version 2". The first time
the updated PC picks the Mac, the two pair. As after every update of the
receiver, macOS asks again whether Outloud may record the Mac's audio.

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
