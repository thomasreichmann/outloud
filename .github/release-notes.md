## What's new in 0.4.0

- **Hearing the Mac no longer keeps it awake.** The Mac records only while
  something on it plays, and stops 10 s after the sound does, so it sleeps
  as usual while a PC listens.
- **macOS remembers its permission to record across updates.** The Mac
  receiver is now signed with Outloud's own certificate. Updating the Mac to
  0.4.0 asks once more whether Outloud may record its sound; later updates
  do not.
- Both changes are on the Mac, so update it. 0.4.0 and 0.3 work together,
  so updating one side at a time is fine.

**Coming from 0.2? Update both the PC and the Mac**, with the commands
below: 0.3 and later cannot talk to 0.2. The first time the updated PC picks
the Mac, the two pair.

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
