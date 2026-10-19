package macsetup

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thomasreichmann/outloud/internal/pairing"
)

// serve starts a setup server and returns its URL, reached through
// localhost, and its pin.
func serve(t *testing.T, pc *pairing.Identity, peers *pairing.Store) (base, pin string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range binaries {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("binary "+name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Server{Dir: dir, Identity: pc, Peers: peers, Name: "PC", Logf: t.Logf}
	cmd, err := s.Serve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(cmd)
	if len(f) != 7 || f[0] != "curl" || f[2] != "--pinnedpubkey" || f[5] != "|" {
		t.Fatalf("command %q", cmd)
	}
	u, err := url.Parse(f[4])
	if err != nil {
		t.Fatal(err)
	}
	u.Host = net.JoinHostPort("127.0.0.1", u.Port())
	return u.String(), f[3]
}

// get fetches like the Mac's curl does: any certificate, as long as its
// key is pinned.
func get(t *testing.T, url, pin string) (int, string) {
	t.Helper()
	want, err := pairing.PinID(pin)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if pairing.PeerID(cs) != want {
				return errors.New("pin mismatch")
			}
			return nil
		},
	}}}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func newID(t *testing.T) *pairing.Identity {
	t.Helper()
	id, err := pairing.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSetupPairs(t *testing.T) {
	pc, pcPeers := newID(t), pairing.MemoryStore()
	base, pin := serve(t, pc, pcPeers)
	if pin != pc.Pin() {
		t.Errorf("command pins %s, want %s", pin, pc.Pin())
	}
	status, body := get(t, base, pin)
	if status != http.StatusOK || !strings.Contains(body, `PIN="`+pin+`"`) || !strings.Contains(body, `BASE="https://127.0.0.1:`) {
		t.Fatalf("script: %d\n%s", status, body)
	}
	if status, body := get(t, base+"/outloud-receiver-darwin-arm64.tar.gz", pin); status != http.StatusOK || body != "binary outloud-receiver-darwin-arm64.tar.gz" {
		t.Errorf("binary: %d %q", status, body)
	}
	u, _ := url.Parse(base)
	if status, _ := get(t, u.Scheme+"://"+u.Host+"/wrongtok", pin); status != http.StatusNotFound {
		t.Errorf("a wrong token got %d", status)
	}
	if status, _ := get(t, u.Scheme+"://"+u.Host+"/outloud-receiver-darwin-arm64.tar.gz", pin); status != http.StatusNotFound {
		t.Errorf("no token got %d", status)
	}

	mac, macPeers := newID(t), pairing.MemoryStore()
	name, err := Join(context.Background(), base, pin, "Kitchen Mac", mac, macPeers)
	if err != nil {
		t.Fatal(err)
	}
	if name != "PC" || !macPeers.Paired(pc.ID) {
		t.Errorf("the Mac paired with %q: %v", name, macPeers.Paired(pc.ID))
	}
	peers, _ := pcPeers.List()
	if len(peers) != 1 || peers[0].ID != mac.ID || peers[0].Name != "Kitchen Mac" {
		t.Errorf("the PC paired with %+v", peers)
	}
}

// TestSetupPinMismatch checks that a Mac refuses a server whose key is not
// the one in its command, so someone in the middle gets nothing.
func TestSetupPinMismatch(t *testing.T) {
	base, _ := serve(t, newID(t), pairing.MemoryStore())
	other := newID(t)
	macPeers := pairing.MemoryStore()
	if _, err := Join(context.Background(), base, other.Pin(), "Mac", newID(t), macPeers); err == nil {
		t.Error("joined a PC with a different key")
	}
	if peers, _ := macPeers.List(); len(peers) != 0 {
		t.Errorf("the Mac paired with %+v", peers)
	}
}

func TestReleaseScript(t *testing.T) {
	s := Script("https://github.com/x/releases/download/v1", "")
	if strings.Contains(s, "__") || !strings.Contains(s, `PIN=""`) {
		t.Errorf("script:\n%s", s)
	}
	// It downloads what a PC serves, under the same names as on GitHub.
	for _, name := range binaries {
		arch := strings.TrimSuffix(strings.TrimPrefix(name, "outloud-receiver-darwin-"), ".tar.gz")
		if !strings.Contains(strings.ReplaceAll(s, "$ARCH", arch), "/"+name+`"`) {
			t.Errorf("the script does not download %s", name)
		}
	}
}
