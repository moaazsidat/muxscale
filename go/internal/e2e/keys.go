// Package e2e implements the end-to-end encryption between a muxscale daemon
// and an enrolled device.
//
// The relay is an untrusted forwarder. It sees a routing header and ciphertext,
// and nothing else. That is not a policy promise: it is what this package makes
// true, and what the tests in this package assert.
//
// The design is deliberately boring. Noise IK gives mutual authentication,
// forward secrecy and replay protection with a well-analysed handshake, and
// there are no custom primitives anywhere. The one piece that needs human
// attention is pairing: a short authentication string both sides display, so a
// compromised relay cannot sit in the middle of the very exchange that
// establishes trust.
package e2e

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/flynn/noise"
	"golang.org/x/crypto/curve25519"
)

// KeyPair is an X25519 identity.
type KeyPair struct {
	Public  []byte
	Private []byte
}

// GenerateKeyPair creates a new identity.
//
// On a phone this key belongs in the secure enclave and never reaches Go; this
// implementation is for the daemon side and for tests. The daemon's key is
// written to the state directory at 0600, alongside the tailnet node key.
func GenerateKeyPair() (KeyPair, error) {
	var priv [32]byte
	if _, err := rand.Read(priv[:]); err != nil {
		return KeyPair{}, err
	}
	// Clamp per RFC 7748 so the scalar is a valid X25519 private key.
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64

	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{Public: pub, Private: priv[:]}, nil
}

// PublicB64 renders a public key for QR codes and config files.
func (k KeyPair) PublicB64() string {
	return base64.RawURLEncoding.EncodeToString(k.Public)
}

// ParsePublicKey decodes a base64 public key.
func ParsePublicKey(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		// Tolerate standard base64 too: QR scanners and copy-paste both
		// produce it, and a user should not have to care which.
		b, err = base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("not a valid public key: %w", err)
		}
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("public key is %d bytes, want 32", len(b))
	}
	return b, nil
}

// cipherSuite is X25519 + ChaCha20-Poly1305 + BLAKE2s.
var cipherSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)

// protocolName binds the handshake to this application and version. Two
// muxscale versions that disagree about the protocol will fail to handshake
// rather than negotiate something neither side intended.
var protocolName = []byte("muxscale/1 device-pairing")

// Role identifies which side of the handshake a party is on.
type Role int

const (
	// RoleDaemon is the responder: the host being connected to.
	RoleDaemon Role = iota
	// RoleDevice is the initiator: the phone connecting in.
	RoleDevice
)

// Handshake drives a Noise IK exchange.
//
// IK is the right pattern here because the device already knows the daemon's
// static public key -- it scanned it from the QR code -- so the daemon is
// authenticated on the first message, and the device's own key is transmitted
// encrypted rather than in the clear.
type Handshake struct {
	state *noise.HandshakeState
	role  Role

	send *noise.CipherState
	recv *noise.CipherState
	done bool
}

// NewHandshake starts a handshake.
//
// remoteStatic is required for the device (it knows the daemon's key from
// pairing) and ignored for the daemon (it learns the device's key during the
// exchange).
func NewHandshake(role Role, local KeyPair, remoteStatic []byte) (*Handshake, error) {
	if len(local.Private) != 32 || len(local.Public) != 32 {
		return nil, errors.New("local key pair is malformed")
	}
	if role == RoleDevice && len(remoteStatic) != 32 {
		return nil, errors.New("the device must know the daemon's public key before handshaking")
	}

	cfg := noise.Config{
		CipherSuite:   cipherSuite,
		Pattern:       noise.HandshakeIK,
		Initiator:     role == RoleDevice,
		Prologue:      protocolName,
		StaticKeypair: noise.DHKey{Private: local.Private, Public: local.Public},
	}
	if role == RoleDevice {
		cfg.PeerStatic = remoteStatic
	}

	state, err := noise.NewHandshakeState(cfg)
	if err != nil {
		return nil, fmt.Errorf("starting the handshake: %w", err)
	}
	return &Handshake{state: state, role: role}, nil
}

// WriteMessage produces the next handshake message.
func (h *Handshake) WriteMessage(payload []byte) ([]byte, error) {
	if h.done {
		return nil, errors.New("handshake is already complete")
	}
	msg, cs0, cs1, err := h.state.WriteMessage(nil, payload)
	if err != nil {
		return nil, err
	}
	h.maybeFinish(cs0, cs1)
	return msg, nil
}

// ReadMessage consumes a handshake message and returns its payload.
func (h *Handshake) ReadMessage(msg []byte) ([]byte, error) {
	if h.done {
		return nil, errors.New("handshake is already complete")
	}
	payload, cs0, cs1, err := h.state.ReadMessage(nil, msg)
	if err != nil {
		return nil, fmt.Errorf("handshake failed: %w", err)
	}
	h.maybeFinish(cs0, cs1)
	return payload, nil
}

func (h *Handshake) maybeFinish(cs0, cs1 *noise.CipherState) {
	if cs0 == nil || cs1 == nil {
		return
	}
	// The initiator's first cipher state encrypts; the responder's decrypts.
	if h.role == RoleDevice {
		h.send, h.recv = cs0, cs1
	} else {
		h.send, h.recv = cs1, cs0
	}
	h.done = true
}

// Complete reports whether the handshake has finished.
func (h *Handshake) Complete() bool { return h.done }

// PeerStatic returns the other side's public key, known once the handshake
// completes. The daemon uses this to record which device it just paired with.
func (h *Handshake) PeerStatic() []byte { return h.state.PeerStatic() }

// ChannelBinding returns the handshake hash.
//
// This is the value the short authentication string is derived from. It commits
// to both parties' keys and every handshake message, so two sessions with a
// relay in the middle cannot produce the same binding.
func (h *Handshake) ChannelBinding() []byte {
	return h.state.ChannelBinding()
}

// Session is an established encrypted channel.
type Session struct {
	send *noise.CipherState
	recv *noise.CipherState
}

// Session returns the established channel, or an error if the handshake has
// not finished.
func (h *Handshake) Session() (*Session, error) {
	if !h.done {
		return nil, errors.New("handshake is not complete")
	}
	return &Session{send: h.send, recv: h.recv}, nil
}

// Seal encrypts a payload for the peer, binding it to a routing header.
//
// The header is prepended to the plaintext rather than passed as Noise
// additional-authenticated-data. That is deliberate and was not the first
// design: AD in transport messages is a flynn/noise extension, and the Noise
// spec says transport messages carry no AD, so strict implementations -- snow,
// which the Rust client uses -- cannot produce or verify it. Writing the second
// implementation is what surfaced that, and a protocol only one library can
// speak is not a protocol.
//
// The security property is unchanged: a relay that rewrites the outer routing
// header produces a mismatch against this authenticated inner copy, and Open
// rejects it. As a bonus the header is now confidential as well as
// authenticated, so the relay learns nothing from it beyond what it must route
// on.
func (s *Session) Seal(plaintext, header []byte) ([]byte, error) {
	if len(header) > maxHeaderBytes {
		return nil, fmt.Errorf("routing header is %d bytes, over the %d limit",
			len(header), maxHeaderBytes)
	}
	framed := make([]byte, 2+len(header)+len(plaintext))
	binary.BigEndian.PutUint16(framed[:2], uint16(len(header)))
	copy(framed[2:], header)
	copy(framed[2+len(header):], plaintext)
	return s.send.Encrypt(nil, nil, framed)
}

// maxHeaderBytes bounds the framed routing header, so a malformed length
// cannot make Open allocate wildly.
const maxHeaderBytes = 4096

// Open decrypts a payload and verifies it was addressed as received.
//
// wantHeader is the routing header the message actually arrived under. If the
// authenticated copy inside disagrees, the message was redirected in transit
// and is refused.
//
// Noise's nonce sequence provides replay protection: a repeated or reordered
// message fails to decrypt rather than being accepted twice. That matters more
// than usual here, because a replayed message could be an approval.
func (s *Session) Open(ciphertext, wantHeader []byte) ([]byte, error) {
	framed, err := s.recv.Decrypt(nil, nil, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decryption failed (tampered, replayed, or out of order): %w", err)
	}
	if len(framed) < 2 {
		return nil, errors.New("message is too short to carry a routing header")
	}
	n := int(binary.BigEndian.Uint16(framed[:2]))
	if n > maxHeaderBytes || 2+n > len(framed) {
		return nil, errors.New("routing header length is out of range")
	}
	gotHeader := framed[2 : 2+n]
	if subtle.ConstantTimeCompare(gotHeader, wantHeader) != 1 {
		return nil, fmt.Errorf(
			"message was addressed to %q but delivered as %q; it was redirected in transit",
			gotHeader, wantHeader)
	}
	return framed[2+n:], nil
}
