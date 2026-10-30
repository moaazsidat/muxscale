package push

// Sealing a notification's content to one phone.
//
// The phone makes an X25519 key pair when notifications are turned on, keeps
// the private half in its keychain, and gives the public half to the daemon
// over the tailnet. Each notification is sealed with a fresh ephemeral key, so
// the notary and Apple carry bytes only that phone can open.
//
// Format, base64url without padding:
//
//	0x01 | ephemeral public key (32) | ChaCha20-Poly1305 ciphertext and tag
//
// The key and nonce come from HKDF-SHA256 over the X25519 shared secret, with
// salt = ephemeral public key | phone's public key and info = sealInfo. A
// fresh ephemeral key per message is what makes a derived nonce safe.
//
// The phone opens it with CryptoKit (Curve25519, HKDF, ChaChaPoly), all of
// which the app's iOS 16.4 floor has. CryptoKit's HPKE would be the standard
// choice but needs iOS 17.

import (
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	sealVersion = 0x01
	sealInfo    = "muxscale push v1"
)

// Seal encrypts plaintext to the phone's X25519 public key (32 raw bytes).
func Seal(phonePublic []byte, plaintext []byte) (string, error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	return seal(eph, phonePublic, plaintext)
}

// seal takes the ephemeral key, so a test can fix it.
func seal(eph *ecdh.PrivateKey, phonePublic []byte, plaintext []byte) (string, error) {
	pub, err := ecdh.X25519().NewPublicKey(phonePublic)
	if err != nil {
		return "", fmt.Errorf("the phone's push key isn't an X25519 public key: %w", err)
	}
	shared, err := eph.ECDH(pub)
	if err != nil {
		return "", err
	}
	aead, nonce, err := sealKey(shared, eph.PublicKey().Bytes(), phonePublic)
	if err != nil {
		return "", err
	}
	out := append([]byte{sealVersion}, eph.PublicKey().Bytes()...)
	out = aead.Seal(out, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// Open is the phone's half, here for tests and for `muxscale debug push`.
func Open(phonePrivate *ecdh.PrivateKey, sealed string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return nil, fmt.Errorf("not base64url: %w", err)
	}
	if len(raw) < 1+32+chacha20poly1305.Overhead || raw[0] != sealVersion {
		return nil, errors.New("not a sealed notification this version understands")
	}
	eph, err := ecdh.X25519().NewPublicKey(raw[1:33])
	if err != nil {
		return nil, err
	}
	shared, err := phonePrivate.ECDH(eph)
	if err != nil {
		return nil, err
	}
	aead, nonce, err := sealKey(shared, raw[1:33], phonePrivate.PublicKey().Bytes())
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, raw[33:], nil)
}

func sealKey(shared, ephPublic, phonePublic []byte) (cipher.AEAD, []byte, error) {
	salt := append(append([]byte{}, ephPublic...), phonePublic...)
	okm, err := hkdf.Key(sha256.New, shared, salt, sealInfo, chacha20poly1305.KeySize+chacha20poly1305.NonceSize)
	if err != nil {
		return nil, nil, err
	}
	aead, err := chacha20poly1305.New(okm[:chacha20poly1305.KeySize])
	if err != nil {
		return nil, nil, err
	}
	return aead, okm[chacha20poly1305.KeySize:], nil
}
