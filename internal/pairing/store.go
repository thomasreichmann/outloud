package pairing

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// StoreFile holds a device's paired devices, in the data directory.
const StoreFile = "paired.json"

// Peer is a paired device.
type Peer struct {
	ID     string    `json:"id"`
	Name   string    `json:"name"`
	Paired time.Time `json:"paired"`
}

// Store is the list of paired devices. Its file is read on every use, so a
// running receiver notices when "outloud-receiver forget" changes it.
type Store struct {
	path string // empty keeps the list in memory
	mu   sync.Mutex
	mem  []Peer
}

// OpenStore uses the list in dir.
func OpenStore(dir string) *Store { return &Store{path: filepath.Join(dir, StoreFile)} }

// MemoryStore keeps a list that is not saved anywhere, for tests.
func MemoryStore(peers ...Peer) *Store { return &Store{mem: peers} }

// List returns the paired devices, oldest first. A nil Store pairs nobody.
func (s *Store) List() ([]Peer, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Paired reports whether the device with this ID is paired. A list that
// cannot be read pairs nobody.
func (s *Store) Paired(id string) bool {
	peers, _ := s.List()
	return slices.ContainsFunc(peers, func(p Peer) bool { return p.ID == id })
}

var errNoStore = errors.New("no list of paired devices")

// Add pairs p, replacing an earlier entry for the same device.
func (s *Store) Add(p Peer) error {
	if s == nil {
		return errNoStore
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	peers, err := s.load()
	if err != nil {
		return err
	}
	peers = slices.DeleteFunc(peers, func(q Peer) bool { return q.ID == p.ID })
	return s.save(append(peers, p))
}

// Remove forgets the device with this ID, if it is paired.
func (s *Store) Remove(id string) error {
	if s == nil {
		return errNoStore
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	peers, err := s.load()
	if err != nil {
		return err
	}
	return s.save(slices.DeleteFunc(peers, func(p Peer) bool { return p.ID == id }))
}

// Forget removes the devices that which names: by name, ignoring case, by
// at least the first 8 characters of an ID, or all of them for "all". It
// returns those it removed.
func (s *Store) Forget(which string) ([]Peer, error) {
	if s == nil {
		return nil, errNoStore
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	peers, err := s.load()
	if err != nil {
		return nil, err
	}
	var gone []Peer
	kept := slices.DeleteFunc(peers, func(p Peer) bool {
		match := which == "all" || strings.EqualFold(p.Name, which) ||
			(len(which) >= 8 && strings.HasPrefix(p.ID, strings.ToLower(which)))
		if match {
			gone = append(gone, p)
		}
		return match
	})
	if len(gone) == 0 {
		return nil, nil
	}
	return gone, s.save(kept)
}

func (s *Store) load() ([]Peer, error) {
	if s.path == "" {
		return slices.Clone(s.mem), nil
	}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var peers []Peer
	if err := json.Unmarshal(b, &peers); err != nil {
		return nil, err
	}
	return peers, nil
}

func (s *Store) save(peers []Peer) error {
	if s.path == "" {
		s.mem = peers
		return nil
	}
	if peers == nil {
		peers = []Peer{}
	}
	b, err := json.MarshalIndent(peers, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
