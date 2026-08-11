package push

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"io"
)

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }

// readLimited reads a bounded amount of an error body. Providers can return
// large HTML error pages, and none of it belongs in a log line.
func readLimited(r io.Reader, n int64) string {
	b, _ := io.ReadAll(io.LimitReader(r, n))
	return string(b)
}

// rsaSignPKCS1v15 signs a SHA-256 digest, for FCM service-account assertions.
func rsaSignPKCS1v15(key *rsa.PrivateKey, digest []byte) ([]byte, error) {
	return rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest)
}

// cryptoSHA256 exposes the hash identifier for tests verifying RS256.
func cryptoSHA256() crypto.Hash { return crypto.SHA256 }
