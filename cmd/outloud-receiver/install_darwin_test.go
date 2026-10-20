package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestIdentifiers pins what macOS knows the receiver by. Its permission to
// record belongs to the bundle identifier (and the signing certificate), and
// each release must replace the login item and the app an older one
// installed, not add its own. See COMPATIBILITY.md.
func TestIdentifiers(t *testing.T) {
	const id = "io.github.thomasreichmann.outloud"
	if agentLabel != id {
		t.Errorf("login item %q, want %q", agentLabel, id)
	}
	if plist := string(infoPlist()); !strings.Contains(plist, "<key>CFBundleIdentifier</key>\n\t<string>"+id+"</string>") {
		t.Errorf("Info.plist does not name the bundle %s:\n%s", id, plist)
	}
	p, err := installPaths()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ got, want string }{
		{p.dir, "Library/Application Support/Outloud"},
		{p.app, "Library/Application Support/Outloud/Outloud.app"},
		{p.binary, "Library/Application Support/Outloud/Outloud.app/Contents/MacOS/outloud-receiver"},
		{p.link, "Library/Application Support/Outloud/outloud-receiver"},
		{p.plist, "Library/LaunchAgents/" + id + ".plist"},
		{p.log, "Library/Logs/Outloud.log"},
	} {
		if !strings.HasSuffix(filepath.ToSlash(c.got), "/"+c.want) {
			t.Errorf("%s, want ~/%s", c.got, c.want)
		}
	}
}
