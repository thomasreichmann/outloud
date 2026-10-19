package pairing

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/thomasreichmann/outloud/internal/proto"
)

// Timeout bounds a whole pairing, including the time people take to answer.
const Timeout = 3 * time.Minute

const nonceSize = 32

var (
	// ErrDeclined means the other side did not agree to pair.
	ErrDeclined = errors.New("pairing was declined")
	// ErrCancelled means this side did not agree to pair.
	ErrCancelled = errors.New("pairing was cancelled")
)

// The code both sides show is a hash of both keys and of a fresh nonce from
// each side. The connecting side commits to its nonce before it sees the
// other's, and the other side picks its nonce before it sees the first: so a
// device in the middle, holding a connection to each side, cannot choose
// nonces that make the two codes it causes come out equal, and has one
// chance in a million of that happening by luck. Including the keys makes
// those codes differ, since each side sees the middle device's key instead
// of the other side's.
func code(initiator, responder string, ni, nr []byte) string {
	h := sha256.New()
	io.WriteString(h, "Outloud pairing code\x00"+initiator+"\x00"+responder+"\x00")
	h.Write(ni)
	h.Write(nr)
	n := binary.BigEndian.Uint64(h.Sum(nil)) % 1_000_000
	return fmt.Sprintf("%03d %03d", n/1000, n%1000)
}

func nonce() []byte {
	b := make([]byte, nonceSize)
	rand.Read(b)
	return b
}

// Initiate runs the connecting side of the code exchange, after the Hellos,
// and returns the code to show. self and peer are the IDs of this device and
// of the one it connected to, as seen in the TLS handshake.
func Initiate(w io.Writer, rd *proto.Reader, self, peer string) (string, error) {
	ni := nonce()
	commit := sha256.Sum256(ni)
	if err := proto.WriteMsg(w, proto.MsgPairCommit, commit[:]); err != nil {
		return "", err
	}
	nr, err := expect(rd, proto.MsgPairNonce, nonceSize)
	if err != nil {
		return "", err
	}
	if err := proto.WriteMsg(w, proto.MsgPairReveal, ni); err != nil {
		return "", err
	}
	return code(self, peer, ni, nr), nil
}

// Respond runs the accepting side of the code exchange and returns the code
// to show.
func Respond(w io.Writer, rd *proto.Reader, self, peer string) (string, error) {
	commit, err := expect(rd, proto.MsgPairCommit, sha256.Size)
	if err != nil {
		return "", err
	}
	nr := nonce()
	if err := proto.WriteMsg(w, proto.MsgPairNonce, nr); err != nil {
		return "", err
	}
	ni, err := expect(rd, proto.MsgPairReveal, nonceSize)
	if err != nil {
		return "", err
	}
	if sum := sha256.Sum256(ni); !bytes.Equal(sum[:], commit) {
		return "", errors.New("pairing: the peer's nonce does not match its commitment")
	}
	return code(peer, self, ni, nr), nil
}

// Agree asks this side's user, through ask, whether both codes match, tells
// the peer the answer and waits for the peer's. It returns nil if both said
// yes, ErrCancelled if this side said no and ErrDeclined if the other did.
// ask must return soon after cancel is closed, which happens when the peer
// says no or the connection fails before this side has answered.
func Agree(w io.Writer, rd *proto.Reader, ask func(cancel <-chan struct{}) bool) error {
	peer := make(chan error, 1)
	go func() {
		p, err := expect(rd, proto.MsgPairDecision, 1)
		switch {
		case err != nil:
			peer <- err
		case p[0] != 1:
			peer <- ErrDeclined
		default:
			peer <- nil
		}
	}()
	cancel := make(chan struct{})
	local := make(chan bool, 1)
	go func() { local <- ask(cancel) }()

	answered := false
	for {
		select {
		case err := <-peer:
			if err != nil {
				close(cancel)
				<-local
				return err
			}
			// The peer said yes; only this side's answer is missing.
			peer, answered = nil, true
		case yes := <-local:
			decision := []byte{0}
			if yes {
				decision[0] = 1
			}
			err := proto.WriteMsg(w, proto.MsgPairDecision, decision)
			if !yes {
				return ErrCancelled
			}
			if err != nil {
				return err
			}
			if answered {
				return nil
			}
			return <-peer
		}
	}
}

func expect(rd *proto.Reader, t proto.MsgType, size int) ([]byte, error) {
	got, p, err := rd.Next()
	if err != nil {
		return nil, err
	}
	if got != t || len(p) != size {
		return nil, fmt.Errorf("pairing: expected %v, got %v of %d bytes", t, got, len(p))
	}
	return bytes.Clone(p), nil
}
