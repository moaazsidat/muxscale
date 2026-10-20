package builtin

import (
	"testing"

	"github.com/muxscale/muxscale/go/internal/adapter"
)

// Which agents show their queue, and how. A change here is a change to what
// the README promises; update both.
func TestQueueCoverage(t *testing.T) {
	screen := map[string]bool{"codex": true, "gemini-cli": true, "pi": true, "kimi": true}
	for _, a := range reg(t).All() {
		id := a.Info().ID
		_, reads := a.(adapter.ScreenQueueReader)
		if reads != screen[id] {
			t.Errorf("%s: reads its queue off the screen = %v, want %v", id, reads, screen[id])
		}
	}
}
