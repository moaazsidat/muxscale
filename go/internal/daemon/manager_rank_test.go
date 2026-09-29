package daemon

import (
	"testing"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Regression: the discovery scan set every discovered session to "transcript"
// unconditionally, so an adoption made on one tick was undone on the next --
// three seconds later, silently. The session stayed writable in the manager's
// own map and reported read-only on the wire, which is the worst of both: the
// app hid the compose box for a session that could have accepted input.
//
// The rule is that a scan may never lower a session's attachment.
func TestAttachRankNeverLetsAScanDowngrade(t *testing.T) {
	better := []protocol.Attachment{
		protocol.AttachNative,
		protocol.AttachWrap,
		protocol.AttachTmux,
		protocol.AttachTermctl,
	}
	for _, a := range better {
		if attachRank(a) <= attachRank(protocol.AttachTranscript) {
			t.Errorf("%s does not outrank transcript, so a rescan would discard it", a)
		}
	}
	// Ordering among the writable ones matters too: a live PTY must not be
	// traded for a 2 Hz sample of someone else's window.
	if attachRank(protocol.AttachTermctl) >= attachRank(protocol.AttachTmux) {
		t.Error("termctl must rank below tmux")
	}
	if attachRank(protocol.AttachTmux) >= attachRank(protocol.AttachWrap) {
		t.Error("tmux must rank below a wrapped pty")
	}
	if attachRank("") != 0 {
		t.Error("an unset attachment must rank lowest")
	}
}
