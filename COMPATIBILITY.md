# Compatibility

From 1.0 on, every Outloud 1.x release works with every other. Any 1.x PC
works with any 1.x Mac, and each release reads the files an older one left
behind. This file lists what that fixes, and how things may still change.
The tests named under each part fail if any of it changes by accident.

## What stays as it is

### The protocol

Tests: `internal/proto/compat_test.go`, `internal/pairing/compat_test.go`,
and in `internal/sender/sender_test.go` `TestNewerReceiver`, `TestNewerSender`,
`TestUnknownMessages`, `TestOldSender` and `TestOldReceiver`.

**Connection.** A sender connects to the receiver over TCP, by default on
port 47811. Both sides write the 4 bytes `OUT2`. Then comes TLS 1.3, in which
both present a self-signed certificate for an ECDSA P-256 key. A device's ID
is the hex SHA-256 of its public key (the DER SubjectPublicKeyInfo). Each
side checks the other's ID against its paired devices, not against
certificate authorities.

**Messages.** Each message is a type byte, then the length of the payload as
a little-endian uint32, then the payload, at most 1 MiB.

| type | name | payload |
|---|---|---|
| 1 | hello | JSON Hello; each side sends one first |
| 2 | format | sample rate (uint32 LE), channels (1 byte, 1 or 2); comes before the first audio |
| 3 | audio | interleaved signed 16-bit little-endian PCM in the last format |
| 4 | idle | none; the sound stopped |
| 5 | volume | gain in [0, 1], float32 LE |
| 6 | ping | none |
| 7 | stats | JSON Stats, from the side playing the audio |
| 8–11 | pair commit, nonce, reveal, decision | see pairing |
| 12 | capture | JSON Capture: a problem with the sending side's capture; a format message means it is over |

The side sending audio writes something at least every second, a ping if
nothing else. The side playing it writes stats every second. Either side may
drop a connection that has been silent for 5 s.

**Hello.** All fields but `version` and `name` are optional, and are left out
when empty.

| field | from | meaning |
|---|---|---|
| `version` | both | protocol version, 2 |
| `name` | both | the device's name |
| `software` | both | program and release, for logs |
| `latency_ms` | sender | buffer to keep; 0 means the receiver's default |
| `listen` | sender | stream the receiver's own sound back instead |
| `pair` | sender | pair instead; the pairing exchange follows |
| `if_free` | sender | do not take over from another device; refuse with `in use` |
| `refused` | receiver | why the receiver turns the sender away; the connection then ends |
| `user` | receiver | with `in use`, the device using the receiver |

The reasons for refusing are `not paired`, `cannot capture`, `busy` (pairing
with someone else), `in use` and `version`. The capture problems are `needs
permission` and `denied`.

**Pairing.**
1. The sender writes SHA-256 of its 32-byte nonce Ni.
2. The receiver writes its 32-byte nonce Nr.
3. The sender writes Ni.
4. Both show a code made from the first 8 bytes of SHA-256(`Outloud pairing
   code` NUL sender ID NUL receiver ID NUL Ni Nr). The bytes are read as a
   big-endian integer, taken mod 1,000,000, and shown as `123 456`.
5. Each side writes one byte, 1 for yes.

**Discovery.** Receivers advertise `_outloud._tcp` through DNS-SD. The TXT
record holds `v=2`, the protocol version, and `capture=1` when the receiver
can share its sound.

### Files and names

Tests: `cmd/outloud/config_test.go`, `internal/pairing/compat_test.go` and
`cmd/outloud-receiver/install_darwin_test.go`.

**On the PC:**
- **Settings, key and paired Macs:** `config.json`, `identity.pem` (PKCS #8
  PEM) and `paired.json` in `%APPDATA%\Outloud`.
- **Log:** `%LOCALAPPDATA%\Outloud\Outloud.log`.
- **Programs:** `%LOCALAPPDATA%\Programs\Outloud`.
- **Registry:** the `Outloud` value under `HKCU\...\Run`, and the `Outloud`
  uninstall key.
- **Start menu:** `Outloud.lnk`.

**On the Mac:**
- **App, key and paired PCs:** `Outloud.app`, `identity.pem` and `paired.json`
  in `~/Library/Application Support/Outloud`.
- **Login item:** `io.github.thomasreichmann.outloud`.
- **Log:** `~/Library/Logs/Outloud.log`.
- **App identity:** bundle identifier `io.github.thomasreichmann.outloud`,
  signed with Outloud's certificate. `build.sh` checks the certificate's
  SHA-1. macOS ties its permission to record to the two of them together.

**Releases:** `install-windows.ps1`, `install-mac.sh`,
`Outloud-<version>-windows-x64.zip`,
`outloud-receiver-darwin-arm64.tar.gz` and `-amd64.tar.gz`, and `SHA256SUMS`.

## How it may change

- **Optional fields** may be added to the Hello, stats, capture problems and
  settings. A missing field must mean what an older release does, or, for a
  setting, its default. Older releases ignore fields they do not know; when
  they save settings, they drop them.
- **Message types** may be added. Peers skip types they do not know. A side
  sends a new type only where skipping it is harmless, or once the other side
  has said it understands it, in its Hello or TXT record.
- **Refusal reasons and capture problems** may be added. Older releases show
  their text.
- **TXT keys, settings and command-line flags** may be added.

Nothing listed above is renamed, removed, renumbered or given another
meaning. That includes encodings, the code computation, the key format and
IDs, paths, identifiers and the signing certificate.

A change that needs any of that is a new protocol version, and a 2.0. The
1.x releases already say which side to update:
- A PC shows a Mac that advertises a newer version as "(update this PC)".
- A PC says the same when the Mac's Hello is newer.
- A receiver answers a newer sender with its own Hello, refusing `version`.
- A receiver that hangs up on `OUT2`, as before version 2, shows as needing
  an update.

## Not covered

These work only between copies of the same release, so they may change:
- the control socket between `outloud-receiver allow` and the running
  receiver;
- the routes of the setup server, since a PC serves its own receiver;
- log and status text.
