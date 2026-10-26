package daemon

import (
	"testing"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// A client that falls behind is cut off rather than quietly skipped: a phone
// that missed a session coming back to life showed it as ended for as long as
// its stream stayed up. Cut off, it reconnects and reads every session afresh.
func TestASubscriberThatFallsBehindIsCutOffNotSkipped(t *testing.T) {
	reg := NewRegistry()
	client, cancelClient := reg.Subscribe()
	defer cancelClient()
	internal, cancelInternal := reg.SubscribeInternal()
	defer cancelInternal()

	s := reg.Upsert("claude-code", "k", nil)
	for i := 0; i < 400; i++ {
		reg.SetState(s.ID, protocol.StateGuess{State: protocol.StateRunning})
		reg.SetState(s.ID, protocol.StateGuess{State: protocol.StateWaitingInput})
	}

	// The client's channel delivers what fitted, then ends.
	n := 0
	for range client {
		n++
	}
	if n == 0 || n > 256 {
		t.Fatalf("client got %d changes before its stream ended, want 1..256", n)
	}

	// The daemon's own consumer keeps its channel: it has no reconnect.
	for i := 0; i < 256; i++ {
		<-internal
	}
	reg.SetState(s.ID, protocol.StateGuess{State: protocol.StateRunning})
	if c, ok := <-internal; !ok || c.Session == nil || c.Session.State != protocol.StateRunning {
		t.Fatalf("internal subscriber after a full buffer: ok=%v change=%+v", ok, c)
	}
}
