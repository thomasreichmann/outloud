package main

import (
	"bytes"
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

	"github.com/thomasreichmann/outloud/internal/proto"
	"github.com/thomasreichmann/outloud/internal/version"
)

const agentLabel = "io.github.thomasreichmann.outloud"

type paths struct {
	dir, binary, plist, log string
}

func installPaths() (paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, err
	}
	dir := filepath.Join(home, "Library", "Application Support", "Outloud")
	return paths{
		dir:    dir,
		binary: filepath.Join(dir, "outloud-receiver"),
		plist:  filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist"),
		log:    filepath.Join(home, "Library", "Logs", "Outloud.log"),
	}, nil
}

// install copies this binary to Application Support and registers a
// LaunchAgent that runs it at login, with args as its flags.
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
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		return err
	}
	if self != p.binary {
		// Replace by rename, never in place: macOS caches code signatures
		// per file and kills a binary that was overwritten while cached.
		if err := copyFile(self, p.binary+".new"); err != nil {
			return err
		}
		if err := os.Rename(p.binary+".new", p.binary); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(p.plist), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p.plist, plist(append([]string{p.binary}, args...), p.log), 0o644); err != nil {
		return err
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
	fmt.Printf("Installed %s. The receiver is running now and starts at every login.\n  binary: %s\n  log:    %s\n",
		version.String(), p.binary, p.log)
	fmt.Printf("\nIf macOS asks whether outloud-receiver may accept incoming connections or\n" +
		"use the local network, allow it.\n")
	if a := addresses(proto.DefaultPort); len(a) > 0 {
		fmt.Printf("If this Mac does not appear among the PC's speakers, use \"Connect by address\"\n"+
			"there with: %s\n", strings.Join(a, " or "))
	}
	fmt.Printf("To uninstall: %q uninstall\n", p.binary)
	return nil
}

func uninstall() error {
	p, err := installPaths()
	if err != nil {
		return err
	}
	_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+agentLabel).Run()
	var errs []error
	for _, f := range []string{p.plist, p.binary} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	_ = os.Remove(p.dir) // only if empty
	if err := errors.Join(errs...); err != nil {
		return err
	}
	fmt.Println("Uninstalled.")
	return nil
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

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
