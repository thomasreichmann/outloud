// Package macsetup serves the receiver to a Mac over the local network, so
// installing it takes one Terminal command and no file copying. Files fetched
// with curl carry no quarantine flag, so Gatekeeper does not block them.
//
// The command pins the PC's key, so curl on the Mac only accepts files from
// this PC, and it carries a random token, which only someone who has the
// command can present. The installer then pairs the Mac and the PC: it
// connects back with the Mac's own key and the token, and each side
// remembers the other.
package macsetup

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	_ "embed"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thomasreichmann/outloud/internal/pairing"
)

//go:embed install.sh
var script string

// DefaultPort is where the installer is served.
const DefaultPort = 8088

// binaries are the receiver's app bundle for each kind of Mac, archived.
var binaries = []string{"outloud-receiver-darwin-arm64.tar.gz", "outloud-receiver-darwin-amd64.tar.gz"}

// Dir is where the Mac receivers are expected: a "mac" folder next to the
// running executable.
func Dir() string {
	exe, err := os.Executable()
	if err != nil {
		return "mac"
	}
	return filepath.Join(filepath.Dir(exe), "mac")
}

// Script is the installer with base as where it downloads from and pin as
// the key that server must have; an empty pin trusts certificate
// authorities, as for GitHub.
func Script(base, pin string) string {
	return strings.NewReplacer("__BASE__", base, "__PIN__", pin).Replace(script)
}

// hello is what the PC and the Mac tell each other when pairing during
// setup.
type hello struct {
	Name string `json:"name"`
}

// Server serves the installer to Macs and pairs with those that run it.
type Server struct {
	Port int
	// Dir holds the Mac binaries; empty means Dir().
	Dir string
	// Identity is the PC's key, which the command pins, and Peers its paired
	// devices, to which Macs that run the command are added. Name is the
	// name those Macs know the PC by.
	Identity *pairing.Identity
	Peers    *pairing.Store
	Name     string
	Logf     func(format string, args ...any)
	// OnPaired, if set, is called for each Mac that pairs.
	OnPaired func(pairing.Peer)
}

// Serve serves the installer until ctx is done and returns the command to
// run on the Mac.
func (s *Server) Serve(ctx context.Context) (string, error) {
	dir := s.Dir
	if dir == "" {
		dir = Dir()
	}
	for _, name := range binaries {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return "", fmt.Errorf("the Mac receiver is missing: %w", err)
		}
	}
	l, err := net.Listen("tcp", ":"+strconv.Itoa(s.Port))
	if err != nil {
		return "", err
	}
	token := newToken()
	tlsConfig := pairing.ServerConfig(s.Identity)
	// curl presents no key of its own, and LibreSSL's TLS 1.3 support is
	// recent; the Mac's receiver does present one when it pairs.
	tlsConfig.ClientAuth = tls.RequestClientCert
	tlsConfig.MinVersion = tls.VersionTLS12
	h := s.handler(dir, token)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.Logf("installer: %s %s from %s", r.Method, strings.Replace(r.URL.Path, token, "…", 1), r.RemoteAddr)
			h.ServeHTTP(w, r)
		}),
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.ServeTLS(l, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.Logf("installer: %v", err)
		}
	}()
	context.AfterFunc(ctx, func() { srv.Close() })
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return Command(net.JoinHostPort(LANIP(), port), s.Identity.Pin(), token), nil
}

// newToken is 40 random bits, short enough to type.
func newToken() string {
	b := make([]byte, 5)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.EncodeToString(b))
}

func (s *Server) handler(dir, token string) http.Handler {
	mux := http.NewServeMux()
	prefix := "/" + token
	mux.HandleFunc("GET "+prefix, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, Script("https://"+r.Host+prefix, s.Identity.Pin()))
	})
	for _, name := range binaries {
		path := filepath.Join(dir, name)
		mux.HandleFunc("GET "+prefix+"/"+name, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeFile(w, r, path)
		})
	}
	mux.HandleFunc("POST "+prefix+"/pair", func(w http.ResponseWriter, r *http.Request) {
		id := pairing.PeerID(*r.TLS)
		var mac hello
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&mac); err != nil || id == "" {
			http.Error(w, "the Mac's key and name are needed", http.StatusBadRequest)
			return
		}
		peer := pairing.Peer{ID: id, Name: mac.Name, Paired: time.Now()}
		if err := s.Peers.Add(peer); err != nil {
			s.Logf("installer: pairing with %q: %v", mac.Name, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.Logf("installer: paired with %q", mac.Name)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(hello{Name: s.Name})
		if s.OnPaired != nil {
			s.OnPaired(peer)
		}
	})
	return mux
}

// Command is what to paste into Terminal on the Mac.
func Command(hostPort, pin, token string) string {
	return "curl -fsSLk --pinnedpubkey " + pin + " https://" + hostPort + "/" + token + " | sh"
}

// Join is the Mac's side of pairing during setup. base and pin come from
// the installer that the PC served; name is what this Mac's receiver
// advertises. It returns the PC's name once both sides remember each other.
func Join(ctx context.Context, base, pin, name string, id *pairing.Identity, peers *pairing.Store) (string, error) {
	pcID, err := pairing.PinID(pin)
	if err != nil {
		return "", err
	}
	tlsConfig := pairing.ClientConfig(id)
	tlsConfig.MinVersion = tls.VersionTLS12
	tlsConfig.VerifyConnection = func(cs tls.ConnectionState) error {
		if pairing.PeerID(cs) != pcID {
			return errors.New("the PC's key does not match the setup command")
		}
		return nil
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}, Timeout: 20 * time.Second}
	body, _ := json.Marshal(hello{Name: name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/pair", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("the PC answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var pc hello
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&pc); err != nil {
		return "", err
	}
	return pc.Name, peers.Add(pairing.Peer{ID: pcID, Name: pc.Name, Paired: time.Now()})
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
