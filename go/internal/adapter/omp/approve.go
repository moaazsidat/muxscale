package omp

import (
	"fmt"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// omp's approval prompt docks above its composer and takes its answer from the
// keyboard (docs/approval-mode.md, docs/keybindings.md in oh-my-pi):
//
//   - Ctrl+Q approves this call outright (app.approval.approve; Ctrl+Enter is
//     the other default, but most terminals cannot tell it from Enter).
//   - Esc denies.
//
// Neither depends on what is highlighted or on a draft in the composer, which
// Enter does: with a draft, Enter sends it. Both are ignored for the first
// half second a prompt is up, which a phone's answer never beats.
//
// These are omp's defaults. Someone who has rebound app.approval.approve, or
// who has Vim mode on (where the first Esc leaves Insert mode), will find the
// phone's answer does not land; the prompt then stays open, the extension
// never reports it closed, and the card stays up with the prompt still
// answerable at the keyboard.
const (
	keyApprove = "\x11" // Ctrl+Q
	keyDeny    = "\x1b" // Esc
)

// ApprovalKeys returns omp's keys for a decision.
func (*Adapter) ApprovalKeys(_ protocol.ApprovalRequest, d protocol.ApprovalDecision) ([]byte, error) {
	switch d.Behavior {
	case "allow":
		return []byte(keyApprove), nil
	case "deny":
		return []byte(keyDeny), nil
	}
	return nil, fmt.Errorf("omp's prompt takes allow or deny, not %q", d.Behavior)
}
