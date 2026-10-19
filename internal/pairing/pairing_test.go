package pairing

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/thomasreichmann/outloud/internal/proto"
)

func TestIdentityPersists(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || len(a.ID) != 64 {
		t.Errorf("IDs %q and %q", a.ID, b.ID)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, IdentityFile))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("key file mode %v", fi.Mode().Perm())
		}
	}
	if id, err := PinID(a.Pin()); err != nil || id != a.ID {
		t.Errorf("pin %q gives %q, %v", a.Pin(), id, err)
	}
	if _, err := PinID("sha256//short"); err == nil {
		t.Error("a bad pin was accepted")
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s := OpenStore(dir)
	if s.Paired("x") {
		t.Error("an empty store pairs x")
	}
	for _, p := range []Peer{{ID: "a", Name: "Mac"}, {ID: "b", Name: "Other"}, {ID: "a", Name: "Renamed"}} {
		if err := s.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	peers, err := OpenStore(dir).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 || peers[0].ID != "b" || peers[1].Name != "Renamed" {
		t.Errorf("peers %+v", peers)
	}
	if err := s.Remove("b"); err != nil {
		t.Fatal(err)
	}
	if s.Paired("b") || !s.Paired("a") {
		t.Error("removing b removed the wrong peer")
	}

	s.Add(Peer{ID: "0123456789abcdef", Name: "Office PC"})
	s.Add(Peer{ID: "fedcba9876543210", Name: "Laptop"})
	for _, tc := range []struct {
		which string
		want  int
	}{{"nobody", 0}, {"office pc", 1}, {"fedcba9", 0}, {"FEDCBA98", 1}, {"all", 1}} {
		gone, err := s.Forget(tc.which)
		if err != nil || len(gone) != tc.want {
			t.Errorf("Forget(%q) removed %+v, %v", tc.which, gone, err)
		}
	}
	if peers, _ := s.List(); len(peers) != 0 {
		t.Errorf("left %+v", peers)
	}
}

// pipe is a TLS connection between two new identities. Closing raw ends it
// at once; closing the TLS ends would wait for the goodbye to be read.
func pipe(t *testing.T) (client, server *Identity, cc, sc net.Conn, raw net.Conn) {
	t.Helper()
	var err error
	if client, err = NewIdentity(); err != nil {
		t.Fatal(err)
	}
	if server, err = NewIdentity(); err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	c, s := Client(a, client), Server(b, server)
	done := make(chan error, 1)
	go func() { done <- s.Handshake() }()
	if err := c.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := PeerID(c.ConnectionState()); got != server.ID {
		t.Errorf("client sees %s, want %s", got, server.ID)
	}
	if got := PeerID(s.ConnectionState()); got != client.ID {
		t.Errorf("server sees %s, want %s", got, client.ID)
	}
	return client, server, c, s, a
}

type result struct {
	code string
	err  error
}

func TestCodesMatch(t *testing.T) {
	client, server, cc, sc, _ := pipe(t)
	resp := make(chan result, 1)
	go func() {
		code, err := Respond(sc, proto.NewReader(sc), server.ID, client.ID)
		resp <- result{code, err}
	}()
	got, err := Initiate(cc, proto.NewReader(cc), client.ID, server.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := <-resp
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got != r.code || len(got) != 7 {
		t.Errorf("codes %q and %q", got, r.code)
	}
	ni, nr := nonce(), nonce()
	if code(client.ID, server.ID, ni, nr) == code(client.ID, "someone else", ni, nr) {
		t.Error("the code does not depend on the keys")
	}
}

// agree runs Agree on both ends of a connection with the given answers. A
// side whose answer is nil waits until it is cancelled.
func agree(t *testing.T, client, server *bool) (clientErr, serverErr error, serverCancelled bool) {
	t.Helper()
	_, _, cc, sc, raw := pipe(t)
	ask := func(answer *bool, cancelled *bool) func(<-chan struct{}) bool {
		return func(cancel <-chan struct{}) bool {
			if answer != nil {
				return *answer
			}
			select {
			case <-cancel:
				*cancelled = true
			case <-time.After(5 * time.Second):
				t.Error("ask was not cancelled")
			}
			return false
		}
	}
	var clientCancelled bool
	done := make(chan error, 1)
	go func() { done <- Agree(sc, proto.NewReader(sc), ask(server, &serverCancelled)) }()
	clientErr = Agree(cc, proto.NewReader(cc), ask(client, &clientCancelled))
	if clientErr != nil {
		raw.Close()
	}
	return clientErr, <-done, serverCancelled
}

func TestAgree(t *testing.T) {
	yes, no := true, false
	if c, s, _ := agree(t, &yes, &yes); c != nil || s != nil {
		t.Errorf("both said yes: %v, %v", c, s)
	}
	if c, s, _ := agree(t, &no, &yes); !errors.Is(c, ErrCancelled) || !errors.Is(s, ErrDeclined) {
		t.Errorf("the client said no: %v, %v", c, s)
	}
	if c, s, _ := agree(t, &yes, &no); !errors.Is(c, ErrDeclined) || !errors.Is(s, ErrCancelled) {
		t.Errorf("the server said no: %v, %v", c, s)
	}
	// The server's user has not answered when the client says no: the
	// server's question goes away.
	c, s, cancelled := agree(t, &no, nil)
	if !errors.Is(c, ErrCancelled) || !errors.Is(s, ErrDeclined) || !cancelled {
		t.Errorf("the client said no first: %v, %v, cancelled %v", c, s, cancelled)
	}
}
