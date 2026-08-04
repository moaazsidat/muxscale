package daemon

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter/claudecode"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// The hook endpoint. Claude Code POSTs here directly via its `http` hook type,
// which is why this exists instead of a subprocess relay: a busy turn fires
// many hooks, and a process spawn per event would add tens of milliseconds and
// a PATH dependency each time.
//
// One rule dominates every path below: FAIL OPEN. Any error, any confusion,
// any timeout returns 200 with an empty body, which Claude Code reads as "no
// decision" and falls through to its own prompt. A monitoring tool that can
// stall the agent it monitors is worse than no monitoring tool.

// HookServer handles agent hook callbacks.
type HookServer struct {
	reg      *Registry
	token    string
	log      *slog.Logger
	holdTime time.Duration
}

// NewHookServer builds the handler. holdTime is how long a decision hook waits
// for a human before giving up and falling through.
func NewHookServer(reg *Registry, token string, hold time.Duration, log *slog.Logger) *HookServer {
	return &HookServer{reg: reg, token: token, holdTime: hold, log: log}
}

// Routes registers handlers on a mux.
func (h *HookServer) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/hook/claude-code", h.handleClaudeCode)
	mux.HandleFunc("/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	})
}

func (h *HookServer) handleClaudeCode(w http.ResponseWriter, r *http.Request) {
	// Every early return is a 200 with an empty object: "no decision".
	if r.Method != http.MethodPost {
		writeJSON(w, claudecode.NoDecision())
		return
	}
	if !h.authorized(r) {
		h.log.Warn("hook request rejected: bad token", "remote", r.RemoteAddr)
		writeJSON(w, claudecode.NoDecision())
		return
	}

	var p claudecode.HookPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&p); err != nil {
		h.log.Warn("hook payload decode failed", "err", err)
		writeJSON(w, claudecode.NoDecision())
		return
	}

	sess := h.applyHook(&p)

	// Decision hooks block here while a human is asked. Everything else has
	// already been recorded and returns immediately.
	switch p.HookEventName {
	case claudecode.HookPermissionRequest, claudecode.HookElicitation:
		writeJSON(w, h.awaitDecision(r, sess, &p))
	default:
		writeJSON(w, claudecode.NoDecision())
	}
}

func (h *HookServer) authorized(r *http.Request) bool {
	if h.token == "" {
		return true // local dev with no token configured
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) == 1
}

// applyHook records the event and updates session state. Returns the session.
func (h *HookServer) applyHook(p *claudecode.HookPayload) *protocol.Session {
	sess := h.reg.Upsert("claude-code", p.SessionID, func(s *protocol.Session) {
		if p.Cwd != "" {
			s.Cwd = p.Cwd
		}
		if p.TranscriptPath != "" {
			s.TranscriptPath = p.TranscriptPath
		}
		if p.PromptID != "" {
			s.PromptID = p.PromptID
		}
		s.Attachment = protocol.AttachNative
		s.Fidelity = protocol.FidelityNativeFull
	})

	now := time.Now()
	guess := func(st protocol.SessionState, reason string) protocol.StateGuess {
		// Hook-sourced state is authoritative: the agent told us, so there is
		// nothing to infer and confidence is exactly 1.
		return protocol.StateGuess{State: st, Confidence: 1.0, Reason: reason,
			Evidence: "hook:" + p.HookEventName, At: now}
	}

	switch p.HookEventName {
	case claudecode.HookSessionStart:
		h.reg.SetState(sess.ID, guess(protocol.StateRunning, "session started"))

	case claudecode.HookUserPromptSubmit:
		h.reg.SetState(sess.ID, guess(protocol.StateRunning, "user submitted a prompt"))

	case claudecode.HookPreToolUse:
		h.reg.AppendEvent(protocol.TranscriptEvent{
			SessionID: sess.ID, Kind: protocol.EventToolCall, At: now,
			ToolName: p.ToolName, ToolUseID: p.ToolUseID, ToolInput: p.ToolInput,
			IsSidechain: p.AgentID != "",
		})
		h.reg.SetState(sess.ID, guess(protocol.StateRunning, "running "+p.ToolName))

	case claudecode.HookPostToolUse, claudecode.HookPostToolUseFail:
		h.reg.AppendEvent(protocol.TranscriptEvent{
			SessionID: sess.ID, Kind: protocol.EventToolResult, At: now,
			ToolName: p.ToolName, ToolUseID: p.ToolUseID,
			IsError:     p.HookEventName == claudecode.HookPostToolUseFail,
			ToolOutput:  p.ToolError,
			IsSidechain: p.AgentID != "",
		})

	case claudecode.HookStop, claudecode.HookStopFailure:
		// The single highest-value signal in the product: the turn is over and
		// the agent is waiting on a human.
		h.reg.SetState(sess.ID, guess(protocol.StateWaitingInput, "turn complete"))

	case claudecode.HookTeammateIdle:
		h.reg.SetState(sess.ID, guess(protocol.StateWaitingInput, "agent idle"))

	case claudecode.HookNotification:
		h.reg.SetState(sess.ID, guess(protocol.StateWaitingInput, "agent requested attention"))

	case claudecode.HookSessionEnd:
		// SessionEnd shares a ~1.5s budget across hooks, so this path must stay
		// trivial. Record and return.
		h.reg.SetState(sess.ID, guess(protocol.StateExited, "session ended"))

	case claudecode.HookPreCompact, claudecode.HookPostCompact:
		h.reg.AppendEvent(protocol.TranscriptEvent{
			SessionID: sess.ID, Kind: protocol.EventCompaction, At: now,
			Text: p.HookEventName,
		})

	case claudecode.HookSubagentStart, claudecode.HookSubagentStop:
		// Recorded for the nested-thread view; does not change parent state.

	default:
		// An event we do not handle yet. Log and move on -- Claude Code adds
		// events regularly and an unknown one is not an error.
		h.log.Debug("unhandled hook event", "event", p.HookEventName)
	}

	return sess
}

// awaitDecision parks the request while a human is asked.
func (h *HookServer) awaitDecision(r *http.Request, sess *protocol.Session, p *claudecode.HookPayload) claudecode.HookResponse {
	req := &protocol.ApprovalRequest{
		ID:        newID(),
		SessionID: sess.ID,
		Source:    protocol.ApprovalNative,
		Kind:      "tool_use",
		ToolName:  p.ToolName,
		ToolInput: p.ToolInput,
		ToolUseID: p.ToolUseID,
		Subject:   p.Subject(),
		Options:   []string{"allow", "deny"},
		PromptID:  p.PromptID,
		AskedAt:   time.Now(),
		Deadline:  time.Now().Add(h.holdTime),
	}
	if p.HookEventName == claudecode.HookElicitation {
		req.Kind = "elicitation"
	}

	h.reg.SetState(sess.ID, protocol.StateGuess{
		State: protocol.StateWaitingApproval, Confidence: 1.0,
		Reason: "approval required: " + req.Subject, Evidence: "hook:" + p.HookEventName,
		At: time.Now(),
	})

	answer := h.reg.AskApproval(req)
	h.log.Info("approval requested", "session", sess.ID, "subject", req.Subject,
		"hold", h.holdTime)

	timer := time.NewTimer(h.holdTime)
	defer timer.Stop()

	select {
	case d := <-answer:
		h.log.Info("approval decided", "session", sess.ID, "behavior", d.Behavior, "by", d.DecidedBy)
		h.reg.SetState(sess.ID, protocol.StateGuess{
			State: protocol.StateRunning, Confidence: 1.0,
			Reason: "approval " + d.Behavior, At: time.Now(),
		})
		return claudecode.PermissionDecision(d.Behavior, d.Reason)

	case <-timer.C:
		// Nobody answered. Return no decision so Claude Code asks locally.
		// This is the correct outcome, not a failure: the human at the keyboard
		// is still a human.
		h.reg.ResolveApproval(req.ID, protocol.ApprovalDecision{Behavior: "", Reason: "timeout"})
		h.clearApprovalState(sess.ID, "approval timed out; agent is asking locally")
		h.log.Info("approval timed out, falling through to local prompt", "session", sess.ID)
		return claudecode.NoDecision()

	case <-r.Context().Done():
		// Claude Code hung up -- typically because the person at the keyboard
		// answered first. Withdraw the phone's pending approval.
		h.reg.ResolveApproval(req.ID, protocol.ApprovalDecision{Behavior: "", Reason: "answered at the keyboard"})
		h.clearApprovalState(sess.ID, "answered at the keyboard")
		h.log.Info("approval withdrawn: agent hung up", "session", sess.ID)
		return claudecode.NoDecision()
	}
}

// clearApprovalState returns a session to RUNNING once an approval stops being
// pending without a remote answer. Leaving it in WAITING_APPROVAL would strand
// the session in a blocked state forever and, worse, keep notifying about a
// question nobody can still answer.
func (h *HookServer) clearApprovalState(sessionID, reason string) {
	h.reg.SetState(sessionID, protocol.StateGuess{
		State: protocol.StateRunning, Confidence: 1.0,
		Reason: reason, Evidence: "approval-withdrawn", At: time.Now(),
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Nothing useful left to do; the agent will read a short body as no
		// decision, which is the safe outcome.
		_ = err
	}
}

// FormatToolInput renders tool arguments for display. The literal arguments are
// shown, never the agent's description of them -- that distinction is the
// structural defence against a repo persuading an agent to ask for something
// harmless-sounding.
func FormatToolInput(in map[string]any) string {
	if len(in) == 0 {
		return ""
	}
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", in)
	}
	return string(b)
}
