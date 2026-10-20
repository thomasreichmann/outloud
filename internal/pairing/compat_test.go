package pairing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests pin what devices keep on disk and agree on. A PC and a Mac
// from different 1.x releases must recognize each other's keys and show the
// same pairing code, and every release must read the files an older one
// wrote. The expected values come from openssl and Python, not from this
// package. See COMPATIBILITY.md.

const goldenKey = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgLw3qc0rRjs5pSBfQ
f86JLu0bTt5KW9TSD52ioV8+3QShRANCAARZWIVzXk3CQpIGxSo6WGmZgAvRaMHT
RQjN7nBuTgaxrXEcs3oRetHu/1dV4wCkvSAmkzqlglFIYuRV3sDgz3BW
-----END PRIVATE KEY-----
`

// TestKeyFormat checks that a key file loads, and that its ID and pin are
// the SHA-256 of the public key, in hex and in curl's form.
func TestKeyFormat(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, IdentityFile), []byte(goldenKey), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := "634da0871a3b83f055ac94056b32c93040668d1fb4d506718b72d876cea9b132"; id.ID != want {
		t.Errorf("ID %s, want %s", id.ID, want)
	}
	if want := "sha256//Y02ghxo7g/BVrJQFazLJMEBmjR+01QZxi3LYds6psTI="; id.Pin() != want {
		t.Errorf("pin %s, want %s", id.Pin(), want)
	}
	if got, err := PinID(id.Pin()); err != nil || got != id.ID {
		t.Errorf("PinID gives %s, %v", got, err)
	}
}

// TestCodeFormat checks the pairing code that both sides compute.
func TestCodeFormat(t *testing.T) {
	if nonceSize != 32 {
		t.Errorf("nonces of %d bytes, want 32", nonceSize)
	}
	ni, nr := make([]byte, 32), make([]byte, 32)
	for i := range 32 {
		ni[i], nr[i] = byte(i), byte(32+i)
	}
	if got := code("initiator-id", "responder-id", ni, nr); got != "733 173" {
		t.Errorf("code %q, want \"733 173\"", got)
	}
}

// TestStoreFormat checks that the paired devices that 0.3.0 wrote are read,
// and that the file keeps that form when written.
func TestStoreFormat(t *testing.T) {
	dir := t.TempDir()
	written := `[
  {
    "id": "548cfecd9ea7437f53f717c1fa6309d4ee510d242972cfc09d3c47cc5daf0660",
    "name": "DESKTOP-DF1L075",
    "paired": "2026-10-19T19:04:20.029618-03:00"
  }
]`
	if err := os.WriteFile(filepath.Join(dir, StoreFile), []byte(written), 0o600); err != nil {
		t.Fatal(err)
	}
	s := OpenStore(dir)
	peers, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 10, 19, 19, 4, 20, 29618000, time.FixedZone("", -3*3600))
	if len(peers) != 1 || peers[0].Name != "DESKTOP-DF1L075" || !peers[0].Paired.Equal(when) ||
		!s.Paired("548cfecd9ea7437f53f717c1fa6309d4ee510d242972cfc09d3c47cc5daf0660") {
		t.Fatalf("read %+v", peers)
	}

	if err := s.Add(Peer{ID: "634da0871a3b83f055ac94056b32c93040668d1fb4d506718b72d876cea9b132", Name: "Mac", Paired: when}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, StoreFile))
	if err != nil {
		t.Fatal(err)
	}
	var file []map[string]any
	if err := json.Unmarshal(b, &file); err != nil || len(file) != 2 {
		t.Fatalf("wrote %s, %v", b, err)
	}
	for _, p := range file {
		if len(p) != 3 || p["id"] == nil || p["name"] == nil || p["paired"] == nil {
			t.Errorf("wrote a peer as %v", p)
		}
	}
}
