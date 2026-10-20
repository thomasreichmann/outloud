## What's new in 0.5.0

- **Two PCs no longer take a Mac from each other in turn.** Choosing a Mac
  that another PC uses takes it over. The other PC then waits, says which
  PC has the Mac, and carries on by itself once the Mac is free. Clicking
  that line in its menu takes the Mac back.
- **Updates come to you.** Once a day the tray asks GitHub whether a new
  release is out, and if so offers to install it, from the top of its menu.
  If the Mac runs an older Outloud, the menu says so and shows how to update
  it. **Check for updates** in the menu turns this off.
- **A version mismatch says which side to update.** This matters for future
  releases: a PC told that its Mac is newer says "update this PC".
- **Requirements, corrected:** Outloud needs macOS 13 or later, and always
  has; earlier notes said 12. The installer now says so on an older Mac.
- **Smaller fixes:**
  - The Mac's log no longer grows without end; it stays under about 2 MB.
  - The tray no longer logs errors when it quits.
  - `outloud-send -name` sets the name a Mac shows for this PC.

**Update both the PC and the Mac.** The takeover fix needs both; 0.5.0
works with 0.3 and 0.4, so one side at a time is fine. Updating a Mac that
runs 0.4.0 keeps its permission to record. Coming from 0.3 or earlier, macOS
asks once more.

**Coming from 0.2?** 0.3 and later cannot talk to 0.2, so update both with
the commands below. The first time the updated PC picks the Mac, the two
pair.

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
