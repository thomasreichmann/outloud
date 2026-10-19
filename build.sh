#!/usr/bin/env bash
# Builds Outloud into dist/Outloud: the Windows programs and their installer,
# plus the Mac receiver in mac/, which the tray app serves to a Mac.
#
#   ./build.sh           build dist/Outloud
#   ./build.sh release   also package dist/release for a GitHub release
#
# VERSION (default: from git describe) is stamped into the binaries.
# Everything is cross-compiled without cgo, so this runs anywhere Go does.
#
# The Mac receiver ships as Outloud.app. With MAC_SIGN_KEYCHAIN set to a
# keychain holding Outloud's certificate (the release does this, on macOS),
# the app is signed with it. macOS ties its permission to record to that
# certificate, so it keeps the permission across updates. Without it the app
# is unsigned, and the receiver signs itself ad hoc when it installs.
set -euo pipefail
cd "$(dirname "$0")"
[ -x "$HOME/.local/sdk/go/bin/go" ] && export PATH="$HOME/.local/sdk/go/bin:$PATH"

repo=thomasreichmann/outloud
version=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
version=${version#v}
base="https://github.com/$repo/releases/download/v$version"
ldflags="-s -w -X github.com/$repo/internal/version.Version=$version"
export CGO_ENABLED=0
# The SHA-1 of Outloud's signing certificate. Signing with any other would
# make every Mac ask again for permission to record, so that fails the build.
mac_cert=80a7a96264740e451fd37f1410a127d24f9b16f7

sign_mac() {
	codesign --force --timestamp=none --keychain "$MAC_SIGN_KEYCHAIN" --sign "$mac_cert" "$1"
	codesign --verify --strict "$1"
	if ! codesign -d -r- "$1" 2>&1 | grep -q "certificate leaf = H\"$mac_cert\""; then
		echo "$1 is not signed with Outloud's certificate" >&2
		exit 1
	fi
}

# Windows resources: the application icon, and a manifest for DPI scaling.
mkdir -p build
go run ./tools/mkicon build/outloud.ico
rsrc="go run github.com/akavel/rsrc@v0.10.2"
$rsrc -arch amd64 -ico build/outloud.ico -manifest cmd/outloud/outloud.manifest -o cmd/outloud/rsrc_windows_amd64.syso
$rsrc -arch amd64 -ico build/outloud.ico -o cmd/outloud-send/rsrc_windows_amd64.syso

out=dist/Outloud
rm -rf dist
mkdir -p "$out/mac"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$ldflags -H windowsgui" -o "$out/Outloud.exe" ./cmd/outloud
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$ldflags" -o "$out/outloud-send.exe" ./cmd/outloud-send
for arch in arm64 amd64; do
	app="build/mac-$arch/Outloud.app"
	rm -rf "build/mac-$arch"
	mkdir -p "$app/Contents/MacOS"
	GOOS=darwin GOARCH=$arch go build -trimpath -ldflags "$ldflags" -o "$app/Contents/MacOS/outloud-receiver" ./cmd/outloud-receiver
	sed "s#__VERSION__#$version#" cmd/outloud-receiver/Info.plist > "$app/Contents/Info.plist"
	if [ -n "${MAC_SIGN_KEYCHAIN:-}" ]; then
		sign_mac "$app"
	fi
	# Without macOS's extended attributes, which would add ._ files.
	COPYFILE_DISABLE=1 tar -czf "$out/mac/outloud-receiver-darwin-$arch.tar.gz" --no-xattrs -C "build/mac-$arch" Outloud.app
done
cp scripts/install.cmd LICENSE "$out/"
sed "s#__RELEASE_BASE__#$base#; s#__VERSION__#$version#" scripts/install.ps1 > "$out/install.ps1"

if [ "${1:-}" = release ]; then
	rel=dist/release
	mkdir -p "$rel"
	zipfile="Outloud-$version-windows-x64.zip"
	if command -v zip > /dev/null; then
		(cd dist && zip -qr "release/$zipfile" Outloud)
	else
		(cd dist && python3 -m zipfile -c "release/$zipfile" Outloud)
	fi
	cp "$out"/mac/outloud-receiver-darwin-* "$rel/"
	cp "$out/install.ps1" "$rel/install-windows.ps1"
	sed "s#__BASE__#$base#; s#__PIN__##" internal/macsetup/install.sh > "$rel/install-mac.sh"
	# macOS has shasum but no sha256sum; both print the same.
	(cd "$rel" && shasum -a 256 -- * > SHA256SUMS)
	ls -l "$rel"
else
	ls -l "$out" "$out/mac"
fi
