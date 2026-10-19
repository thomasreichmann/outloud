// Package macsetup serves the receiver to a Mac over the local network, so
// installing it takes one Terminal command and no file copying. Files fetched
// with curl carry no quarantine flag, so Gatekeeper does not block them.
package macsetup

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed install.sh
var script string

// DefaultPort is where the installer is served.
const DefaultPort = 8088

var binaries = []string{"outloud-receiver-darwin-arm64", "outloud-receiver-darwin-amd64"}

// Dir is where the Mac binaries are expected: a "mac" folder next to the
// running executable.
func Dir() string {
	exe, err := os.Executable()
	if err != nil {
		return "mac"
	}
	return filepath.Join(filepath.Dir(exe), "mac")
}

func handler(dir string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, strings.ReplaceAll(script, "__BASE__", "http://"+r.Host))
	})
	for _, name := range binaries {
		path := filepath.Join(dir, name)
		mux.HandleFunc("GET /"+name, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeFile(w, r, path)
		})
	}
	return mux
}

// Serve serves the installer on port until ctx is done and returns the
// command to run on the Mac.
func Serve(ctx context.Context, port int, logf func(string, ...any)) (string, error) {
	dir := Dir()
	for _, name := range binaries {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return "", fmt.Errorf("the Mac receiver is missing: %w", err)
		}
	}
	l, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return "", err
	}
	h := handler(dir)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logf("installer: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
			h.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logf("installer: %v", err)
		}
	}()
	context.AfterFunc(ctx, func() { srv.Close() })
	return Command(net.JoinHostPort(LANIP(), strconv.Itoa(port))), nil
}

// Command is what to paste into Terminal on the Mac.
func Command(hostPort string) string {
	return "curl -fsSL http://" + hostPort + "/install.sh | sh"
}

// LANIP guesses this machine's address on the local network: the source
// address of the default route. Connecting a UDP socket sends nothing.
func LANIP() string {
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return "localhost"
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}
