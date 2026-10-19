package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thomasreichmann/outloud/internal/macsetup"
	"github.com/thomasreichmann/outloud/internal/pairing"
	"github.com/thomasreichmann/outloud/internal/proto"
	"github.com/thomasreichmann/outloud/internal/version"
)

const agentLabel = "io.github.thomasreichmann.outloud"

type paths struct {
	dir, app, binary, link, plist, log string
}

func installPaths() (paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, err
	}
	dir := filepath.Join(home, "Library", "Application Support", "Outloud")
	app := filepath.Join(dir, "Outloud.app")
	return paths{
		dir: dir,
		// The receiver lives in an app bundle so that macOS can ask for
		// permission to record system audio on its behalf: that needs a
		// bundle identifier and a usage description.
		app:    app,
		binary: filepath.Join(app, "Contents", "MacOS", "outloud-receiver"),
		link:   filepath.Join(dir, "outloud-receiver"),
		plist:  filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist"),
		log:    filepath.Join(home, "Library", "Logs", "Outloud.log"),
	}, nil
}

// install copies the app bundle this binary runs from, or else the binary
// alone, to Application Support, and registers a LaunchAgent that runs it at
// login, with args as its flags.
func install(args []string) error {
	p, err := installPaths()
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	switch app := bundleOf(self); app {
	case p.app:
		// Installed already; this only updates the login item.
	case "":
		// A bare binary, as `go build` makes.
		if err := copyBinary(self, p.binary); err != nil {
			return err
		}
	default:
		if err := copyApp(app, p.app); err != nil {
			return err
		}
	}
	// A release comes signed with Outloud's certificate and stays untouched,
	// so that macOS knows it after an update and keeps its permission to
	// record. Anything else is signed ad hoc.
	if !certSigned(p.app) {
		if err := signAdHoc(p); err != nil {
			return err
		}
	}
	// The plain path, which earlier versions installed to, stays usable.
	if err := os.Remove(p.link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(p.binary, p.link); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.plist), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p.plist, plist(append([]string{p.binary}, args...), p.log), 0o644); err != nil {
		return err
	}
	// The installer that a PC serves names it and pins its key.
	pc := ""
	if pin := os.Getenv("OUTLOUD_SETUP_PIN"); pin != "" {
		if pc, err = joinPC(p.dir, os.Getenv("OUTLOUD_SETUP"), pin, args); err != nil {
			fmt.Printf("warning: could not pair with the PC this came from (%v).\n"+
				"Choose this Mac in Outloud's menu on the PC to pair by comparing a code instead.\n", err)
		}
	}

	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain+"/"+agentLabel).Run()
	// bootout returns before the old job is fully gone, so retry briefly.
	var out []byte
	for range 10 {
		if out, err = exec.Command("launchctl", "bootstrap", domain, p.plist).CombinedOutput(); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, bytes.TrimSpace(out))
	}
	fmt.Printf("Installed %s. The receiver is running now and starts at every login.\n  app: %s\n  log: %s\n",
		version.String(), p.app, p.log)
	if pc != "" {
		fmt.Printf("\nPaired with %s, which can now use this Mac.\n", pc)
	} else {
		fmt.Printf("\nThe first time a PC connects, this Mac asks whether to pair with it. Allow\n" +
			"it if the PC shows the same code.\n")
	}
	fmt.Printf("\nIf macOS asks whether Outloud may accept incoming connections or use the\n" +
		"local network, allow it. When a PC first listens to this Mac, macOS asks\n" +
		"whether Outloud may record this Mac's sound; allow that too.\n")
	if a := addresses(proto.DefaultPort); len(a) > 0 {
		fmt.Printf("If this Mac does not appear among the PC's speakers, use \"Connect by address\"\n"+
			"there with: %s\n", strings.Join(a, " or "))
	}
	fmt.Printf("To uninstall: %q uninstall\n", p.link)
	return nil
}

func uninstall() error {
	p, err := installPaths()
	if err != nil {
		return err
	}
	_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+agentLabel).Run()
	var errs []error
	for _, f := range []string{p.plist, p.link, filepath.Join(p.dir, pairing.IdentityFile), filepath.Join(p.dir, pairing.StoreFile), socketPath()} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := os.RemoveAll(p.app); err != nil {
		errs = append(errs, err)
	}
	_ = os.Remove(p.dir) // only if empty
	if err := errors.Join(errs...); err != nil {
		return err
	}
	fmt.Println("Uninstalled.")
	return nil
}

// joinPC pairs this Mac with the PC whose installer is running, at base
// with key pin, and returns the PC's name. args are the receiver's flags,
// which may name this Mac.
func joinPC(dir, base, pin string, args []string) (string, error) {
	id, err := pairing.LoadIdentity(dir)
	if err != nil {
		return "", err
	}
	name := flagValue(args, "name")
	if name == "" {
		name = computerName()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return macsetup.Join(ctx, base, pin, name, id, pairing.OpenStore(dir))
}

// flagValue finds the value of the flag called name among args.
func flagValue(args []string, name string) string {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		a = strings.TrimLeft(a, "-")
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func plist(args []string, logPath string) []byte {
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + agentLabel + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range args {
		b.WriteString("\t\t<string>" + esc(a) + "</string>\n")
	}
	b.WriteString(`	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<!-- Not Background: background jobs are CPU-throttled, which breaks real-time audio. -->
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>StandardOutPath</key>
	<string>` + esc(logPath) + `</string>
	<key>StandardErrorPath</key>
	<string>` + esc(logPath) + `</string>
</dict>
</plist>
`)
	return b.Bytes()
}

// copyBinary puts a copy of the binary self at to. It replaces what is there
// by rename, never in place: macOS caches code signatures per file and kills
// a binary that was overwritten while cached.
func copyBinary(self, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if err := copyFile(self, to+".new", 0o755); err != nil {
		return err
	}
	return os.Rename(to+".new", to)
}

// signAdHoc completes the installed app bundle and signs it ad hoc, for a
// receiver that does not come signed, such as a development build.
func signAdHoc(p paths) error {
	if err := os.WriteFile(filepath.Join(p.app, "Contents", "Info.plist"), infoPlist(), 0o644); err != nil {
		return err
	}
	// Sign the bundle as a whole (ad hoc), which binds the Info.plist to the
	// binary; macOS then knows the receiver as Outloud in its privacy
	// settings. An ad hoc signature changes with every build, so macOS asks
	// for permission to record again after each update.
	if out, err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", p.app).CombinedOutput(); err != nil {
		fmt.Printf("warning: signing %s: %v: %s\n", p.app, err, bytes.TrimSpace(out))
	}
	return nil
}

// infoPlistTemplate is the app bundle's Info.plist, which build.sh fills in
// for releases too.
//
//go:embed Info.plist
var infoPlistTemplate string

func infoPlist() []byte {
	return []byte(strings.ReplaceAll(infoPlistTemplate, "__VERSION__", strings.TrimPrefix(version.Version, "v")))
}

// bundleOf returns the app bundle that the executable exe is the program of,
// or "" if it is not in one.
func bundleOf(exe string) string {
	macOS := filepath.Dir(exe)
	contents := filepath.Dir(macOS)
	app := filepath.Dir(contents)
	if filepath.Base(macOS) != "MacOS" || filepath.Base(contents) != "Contents" || filepath.Ext(app) != ".app" {
		return ""
	}
	return app
}

// certSigned reports whether app is intact and signed with a certificate,
// as releases are, rather than ad hoc or not at all.
func certSigned(app string) bool {
	if exec.Command("/usr/bin/codesign", "--verify", "--strict", app).Run() != nil {
		return false
	}
	out, err := exec.Command("/usr/bin/codesign", "-dv", app).CombinedOutput()
	return err == nil && !bytes.Contains(out, []byte("Signature=adhoc"))
}

// copyApp puts a copy of the bundle app at to. Like a binary, the bundle is
// replaced by rename, never in place.
func copyApp(app, to string) error {
	tmp, old := to+".new", to+".old"
	_ = os.RemoveAll(tmp)
	err := filepath.WalkDir(app, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(app, path)
		dst := filepath.Join(tmp, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(dst, 0o755)
		case info.Mode().IsRegular():
			return copyFile(path, dst, info.Mode().Perm())
		}
		return fmt.Errorf("unexpected file in %s: %s", app, rel)
	})
	if err != nil {
		return err
	}
	_ = os.RemoveAll(old)
	if err := os.Rename(to, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmp, to); err != nil {
		return err
	}
	return os.RemoveAll(old)
}

func copyFile(from, to string, mode os.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
