// Package pairing lets Outloud devices recognise each other, so that only a
// PC paired with a Mac can play sound on it or hear it.
//
// Each device has its own key pair, made on first use. Pairing means each
// side stores the other's public key. Every connection is TLS in which both
// sides prove they hold their key, which also keeps the audio private; a
// device checks the other's key against its paired devices instead of
// against certificate authorities.
//
// Two devices pair by showing the same six-digit code on both screens and
// asking both users to compare (Initiate, Respond and Agree), or through the
// command that installs the receiver from a PC, which carries the PC's key.
package pairing

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IdentityFile holds a device's private key, in the data directory.
const IdentityFile = "identity.pem"

// Identity is this device's key pair.
type Identity struct {
	// ID is the fingerprint of the public key: the hex SHA-256 of its
	// SubjectPublicKeyInfo.
	ID   string
	cert tls.Certificate
}

// LoadIdentity reads this device's key from dir, making one on first use.
func LoadIdentity(dir string) (*Identity, error) {
	path := filepath.Join(dir, IdentityFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return createIdentity(path)
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s holds no private key", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s holds a %T, not an ECDSA key", path, key)
	}
	return newIdentity(ec)
}

// createIdentity makes a key and saves it at path, unless another process
// got there first, in which case that one is used.
func createIdentity(path string) (*Identity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), IdentityFile+".*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if err := pem.Encode(tmp, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	// A hard link appears complete or not at all, and fails if the file
	// exists. Where links are not supported, a rename is nearly as good.
	if err := os.Link(tmp.Name(), path); errors.Is(err, os.ErrExist) {
		return LoadIdentity(filepath.Dir(path))
	} else if err != nil {
		if err := os.Rename(tmp.Name(), path); err != nil {
			return nil, err
		}
	}
	return newIdentity(key)
}

// NewIdentity makes a key pair that is not saved anywhere, for tests.
func NewIdentity() (*Identity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return newIdentity(key)
}

func newIdentity(key *ecdsa.PrivateKey) (*Identity, error) {
	// The certificate only carries the public key through TLS, so it is
	// made afresh on every start and nobody checks its other fields.
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "Outloud"},
		NotBefore:    time.Now().Add(-24 * time.Hour),
		NotAfter:     time.Now().AddDate(100, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	id, err := fingerprint(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	return &Identity{ID: id, cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}}, nil
}

func fingerprint(pub any) (string, error) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(spki)
	return hex.EncodeToString(sum[:]), nil
}

// Pin is the public key in the form curl's --pinnedpubkey takes.
func (id *Identity) Pin() string {
	b, _ := hex.DecodeString(id.ID)
	return "sha256//" + base64.StdEncoding.EncodeToString(b)
}

// PinID turns a pin, as Pin makes it, back into the ID of the key.
func PinID(pin string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(pin, "sha256//"))
	if err != nil || len(b) != sha256.Size || !strings.HasPrefix(pin, "sha256//") {
		return "", fmt.Errorf("not a key pin: %q", pin)
	}
	return hex.EncodeToString(b), nil
}

// Short abbreviates an ID for logs and lists.
func Short(id string) string { return id[:min(len(id), 12)] }
