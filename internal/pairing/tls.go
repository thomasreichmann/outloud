package pairing

import (
	"crypto/tls"
	"errors"
	"net"
)

// ErrNotPaired means a device turned the other away because they have not
// paired, on one side or both.
var ErrNotPaired = errors.New("not paired")

// ClientConfig is TLS for the side that connects, presenting id. It accepts
// any key from the peer: who that is gets checked after the handshake,
// against the paired devices.
func ClientConfig(id *Identity) *tls.Config {
	return &tls.Config{
		Certificates:       []tls.Certificate{id.cert},
		InsecureSkipVerify: true, // see above; the handshake still proves the peer holds its key
		MinVersion:         tls.VersionTLS13,
	}
}

// ServerConfig is TLS for the side that accepts, presenting id and
// requiring a key from the peer.
func ServerConfig(id *Identity) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{id.cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
	}
}

// Client starts TLS on conn as the side that connected.
func Client(conn net.Conn, id *Identity) *tls.Conn { return tls.Client(conn, ClientConfig(id)) }

// Server starts TLS on conn as the side that accepted.
func Server(conn net.Conn, id *Identity) *tls.Conn { return tls.Server(conn, ServerConfig(id)) }

// PeerID is the ID of the key the peer proved it holds during the
// handshake, or "" if it presented none.
func PeerID(cs tls.ConnectionState) string {
	if len(cs.PeerCertificates) == 0 {
		return ""
	}
	id, _ := fingerprint(cs.PeerCertificates[0].PublicKey)
	return id
}
