package push

import (
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
