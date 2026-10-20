// Package update finds out whether a newer Outloud has been released, and
// compares versions.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/thomasreichmann/outloud/internal/version"
)

const repo = "thomasreichmann/outloud"

// latestURL describes the latest release, in GitHub's API.
var latestURL = "https://api.github.com/repos/" + repo + "/releases/latest"

// Release is a published release.
type Release struct {
	// Version is without the leading "v", like "1.0.0".
	Version string
	// URL is the release's page, with what is new in it.
	URL string
}

// WindowsInstaller is the address of the PowerShell script that installs
// release v on a PC.
func WindowsInstaller(v string) string {
	return "https://github.com/" + repo + "/releases/download/v" + v + "/install-windows.ps1"
}

// MacCommand is the Terminal command that installs the latest release on a
// Mac.
const MacCommand = "curl -fsSL https://github.com/" + repo + "/releases/latest/download/install-mac.sh | sh"

// Latest asks GitHub which release is the latest.
func Latest(ctx context.Context) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", version.String())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var r struct {
		Tag string `json:"tag_name"`
		URL string `json:"html_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return Release{}, fmt.Errorf("reading GitHub's answer: %w", err)
	}
	v := strings.TrimPrefix(r.Tag, "v")
	if _, ok := parse(v); !ok {
		return Release{}, fmt.Errorf("the latest release has an odd name, %q", r.Tag)
	}
	return Release{Version: v, URL: r.URL}, nil
}

// Older reports whether version a comes before release b. Releases are
// numbered like 1.2.3. Anything after the numbers marks a build made after
// that release, such as "1.2.3-4-gabcdef0", so it is not older than it. A
// version that does not start with numbers, such as "dev", is never older.
func Older(a, b string) bool {
	na, okA := parse(a)
	nb, okB := parse(b)
	if !okA || !okB {
		return false
	}
	for i := range na {
		if na[i] != nb[i] {
			return na[i] < nb[i]
		}
	}
	return false
}

// Software returns the version in a peer's Hello.Software, such as
// "Outloud 1.2.3", or "".
func Software(s string) string {
	v, _ := strings.CutPrefix(s, "Outloud ")
	if _, ok := parse(v); !ok {
		return ""
	}
	return v
}

// parse reads the numbers a version starts with.
func parse(v string) ([3]int, bool) {
	var n [3]int
	if end := strings.IndexFunc(v, func(r rune) bool { return r != '.' && (r < '0' || r > '9') }); end >= 0 {
		v = v[:end]
	}
	parts := strings.Split(v, ".")
	if len(parts) > len(n) {
		return n, false
	}
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil {
			return n, false
		}
		n[i] = x
	}
	return n, true
}
