// Command notices writes the licenses of the third-party code in Outloud's
// programs: the Go standard library, and every module that a shipped program
// imports on the platform it is built for. build.sh ships its output next to
// the programs.
package main

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// shipped is what build.sh builds, per platform.
var shipped = []struct {
	goos, goarch string
	pkgs         []string
}{
	{"windows", "amd64", []string{"./cmd/outloud", "./cmd/outloud-send"}},
	{"darwin", "arm64", []string{"./cmd/outloud-receiver"}},
	{"darwin", "amd64", []string{"./cmd/outloud-receiver"}},
}

type module struct {
	path, version, dir string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
}

func run() error {
	mods := map[string]module{}
	for _, s := range shipped {
		out, err := goCmd([]string{"GOOS=" + s.goos, "GOARCH=" + s.goarch, "CGO_ENABLED=0"},
			append([]string{"list", "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}{{end}}"}, s.pkgs...)...)
		if err != nil {
			return err
		}
		for line := range strings.Lines(out) {
			if f := strings.Split(strings.TrimSpace(line), "\t"); len(f) == 3 {
				mods[f[0]] = module{f[0], f[1], f[2]}
			}
		}
	}
	goVersion, err := goCmd(nil, "env", "GOVERSION")
	if err != nil {
		return err
	}
	goRoot, err := goCmd(nil, "env", "GOROOT")
	if err != nil {
		return err
	}
	all := []module{{"Go", strings.TrimSpace(goVersion), strings.TrimSpace(goRoot)}}
	for _, path := range slices.Sorted(maps.Keys(mods)) {
		all = append(all, mods[path])
	}

	var b bytes.Buffer
	b.WriteString("Third-party software in Outloud\n\n" +
		"Outloud's programs are written in Go, and contain the Go standard library\n" +
		"and the modules below. Each is used under the license that follows.\n\n")
	for _, m := range all {
		fmt.Fprintf(&b, "  %s %s\n", m.path, m.version)
	}
	for _, m := range all {
		license, err := find(m.dir, "LICENSE", "LICENSE.md", "LICENSE.txt", "COPYING")
		if err != nil {
			return fmt.Errorf("%s: %w", m.path, err)
		}
		fmt.Fprintf(&b, "\n%s\n%s %s\n\n%s", strings.Repeat("-", 78), m.path, m.version, ensureNewline(license))
		// Apache-licensed work may come with a NOTICE that must be passed on.
		if notice, err := find(m.dir, "NOTICE", "NOTICE.md", "NOTICE.txt"); err == nil {
			fmt.Fprintf(&b, "\n%s", ensureNewline(notice))
		}
	}
	_, err = os.Stdout.Write(b.Bytes())
	return err
}

// find returns the contents of the first of names in dir.
func find(dir string, names ...string) (string, error) {
	for _, name := range names {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			return string(b), nil
		}
	}
	return "", fmt.Errorf("none of %s in %s", strings.Join(names, ", "), dir)
}

func ensureNewline(s string) string {
	s = strings.Trim(s, "\n")
	return s + "\n"
}

func goCmd(env []string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
