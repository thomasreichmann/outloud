#!/bin/sh
# Installs the Outloud receiver on this Mac and starts it at every login.
#
# From a GitHub release:
#   curl -fsSL https://github.com/thomasreichmann/outloud/releases/latest/download/install-mac.sh | sh
# From the PC: the tray app's "Set up a Mac…" serves this script with BASE
# pointing at the PC.
#
# Flags after "sh -s --" are passed on to the receiver, e.g.
#   curl -fsSL .../install-mac.sh | sh -s -- -name "Kitchen"
set -eu
BASE="__BASE__"
case "$(uname -m)" in
	arm64) ARCH=arm64 ;;
	x86_64) ARCH=amd64 ;;
	*) echo "Unsupported Mac: $(uname -m)" >&2; exit 1 ;;
esac
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
echo "Downloading the Outloud receiver ($ARCH) from $BASE ..."
curl -fsSL "$BASE/outloud-receiver-darwin-$ARCH" -o "$TMP/outloud-receiver"
chmod +x "$TMP/outloud-receiver"
"$TMP/outloud-receiver" install "$@"
