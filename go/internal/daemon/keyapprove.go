package daemon

import (
	"errors"
	"fmt"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Approvals with source "hook" (spec §5.7): the agent is asking at its own
// keyboard and has said so through a hook that does not wait for an answer.
// omp is the first. The phone's answer is typed into the agent's terminal as
// the agent's own keys for that prompt, so it is only as good as the keys --
// which is why it is a separate source from "native", where the reply is data.

// approvalShown records a prompt the agent is showing.
func (h *HookServer) approvalShown(sess *protocol.Session, e *adapter.HookEvent) {
	for _, a := range h.reg.SessionApprovals(sess.ID) {
		if a.ToolUseID != "" && a.ToolUseID == e.ToolUseID {
			return // already reported
		}
	}
	subject := e.Subject()
	if e.Reason != "" {
		subject += " (" + e.Reason + ")"
	}
	req := &protocol.ApprovalRequest{
		ID:        newID(),
		SessionID: sess.ID,
		Source:    protocol.ApprovalHook,
		Kind:      "tool_use",
		ToolName:  e.ToolName,
		ToolInput: e.ToolInput,
		ToolUseID: e.ToolUseID,
		Subject:   subject,
		Options:   []string{"allow", "deny"},
		PromptID:  e.PromptID,
		AskedAt:   time.Now(),
		// No deadline: the agent waits at its keyboard for as long as it
		// takes, and nothing here is holding it up.
	}
	// Nothing waits on the answer channel. The answer is typed by
	// AnswerByKeys, and the prompt closing is what retires the request.
	h.reg.AskApproval(req)
	h.reg.SetState(sess.ID, protocol.StateGuess{
		State: protocol.StateWaitingApproval, Confidence: 1.0,
		Reason: "approval required: " + req.Subject, Evidence: "hook:" + e.EventName,
		At: time.Now(),
	})
	h.log.Info("approval shown at the keyboard", "session", sess.ID, "subject", req.Subject)
}

// approvalClosed retires a prompt that has gone away. If the phone answered
// it, the request is already gone and this changes nothing; if the keyboard
// did, the phone's card is withdrawn.
func (h *HookServer) approvalClosed(sess *protocol.Session, e *adapter.HookEvent) {
	pending := h.reg.SessionApprovals(sess.ID)
	left := 0
	for _, a := range pending {
		if a.Source == protocol.ApprovalHook && a.ToolUseID == e.ToolUseID {
			h.reg.ResolveApproval(a.ID, protocol.ApprovalDecision{Reason: "answered at the keyboard"})
			continue
		}
		left++
	}
	if left == 0 {
		reason := "approval denied"
		if e.Approved {
			reason = "approval granted"
		}
		h.reg.SetState(sess.ID, protocol.StateGuess{
			State: protocol.StateRunning, Confidence: 1.0,
			Reason: reason, Evidence: "hook:" + e.EventName, At: time.Now(),
		})
	}
}

// ErrNotFirst refuses an answer to a prompt that is not the one on screen.
var ErrNotFirst = errors.New("an earlier request in this session is still waiting; answer that one first")

// AnswerByKeys types a person's answer to a hook-reported approval into the
// agent's terminal, and retires the request once the keys are delivered.
//
// The keys answer whatever prompt is on screen, so only the oldest pending
// request may be answered: the agent shows its prompts one at a time, in the
// order it asked. A failure to type leaves the request pending, so the phone
// says the answer did not land rather than showing it as done.
func (m *Manager) AnswerByKeys(id string, d protocol.ApprovalDecision, actor string) error {
	if d.Behavior != "allow" && d.Behavior != "deny" {
		return fmt.Errorf(`behavior must be "allow" or "deny"`)
	}
	// One answer at a time: two taps arriving together must not type twice.
	m.answerMu.Lock()
	defer m.answerMu.Unlock()

	req, ok := m.reg.Approval(id)
	if !ok {
		return fmt.Errorf("that request is no longer waiting")
	}
	if pending := m.reg.SessionApprovals(req.SessionID); len(pending) > 0 && pending[0].ID != id {
		return ErrNotFirst
	}
	sess, ok := m.reg.Get(req.SessionID)
	if !ok {
		return fmt.Errorf("unknown session %s", req.SessionID)
	}
	var ka adapter.KeyApprover
	if m.adapters != nil {
		if a, ok := m.adapters.Get(sess.AgentID); ok {
			ka, _ = a.(adapter.KeyApprover)
		}
	}
	if ka == nil {
		return fmt.Errorf("muxscale has no way to answer %s's prompt", sess.AgentID)
	}
	keys, err := ka.ApprovalKeys(req, d)
	if err != nil {
		return err
	}
	if err := m.typeKeys(req.SessionID, keys); err != nil {
		return fmt.Errorf("could not type the answer into the session: %w", err)
	}
	d.DecidedBy = actor
	m.reg.ResolveApproval(id, d)
	_ = m.store.Audit(actor, "approval_typed", req.SessionID,
		fmt.Sprintf("behavior=%s subject=%q", d.Behavior, req.Subject))
	return nil
}
