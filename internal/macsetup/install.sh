#!/bin/sh
# Installs the Outloud receiver on this Mac and starts it at every login.
#
# From a GitHub release:
#   curl -fsSL https://github.com/thomasreichmann/outloud/releases/latest/download/install-mac.sh | sh
# From the PC: the tray app's "Set up a Mac…" serves this script with BASE
# pointing at the PC and PIN set to the PC's key, which curl then insists
# on. The receiver also pairs with that PC.
#
# Flags after "sh -s --" are passed on to the receiver, e.g.
#   curl -fsSL .../install-mac.sh | sh -s -- -name "Kitchen"
set -eu
BASE="__BASE__"
PIN="__PIN__"
case "$(uname -m)" in
	arm64) ARCH=arm64 ;;
	x86_64) ARCH=amd64 ;;
	*) echo "Unsupported Mac: $(uname -m)" >&2; exit 1 ;;
esac
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
echo "Downloading the Outloud receiver ($ARCH) from $BASE ..."
# The archive holds Outloud.app, which installs itself.
if [ -n "$PIN" ]; then
	curl -fsSLk --pinnedpubkey "$PIN" "$BASE/outloud-receiver-darwin-$ARCH.tar.gz" -o "$TMP/receiver.tar.gz"
else
	curl -fsSL "$BASE/outloud-receiver-darwin-$ARCH.tar.gz" -o "$TMP/receiver.tar.gz"
fi
tar -xzf "$TMP/receiver.tar.gz" -C "$TMP"
OUTLOUD_SETUP="$BASE" OUTLOUD_SETUP_PIN="$PIN" "$TMP/Outloud.app/Contents/MacOS/outloud-receiver" install "$@"
