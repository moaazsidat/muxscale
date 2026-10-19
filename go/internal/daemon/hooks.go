package daemon

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/attach"
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
	mgr      *Manager
	token    string
	log      *slog.Logger
	holdTime time.Duration

	// codecs holds one transport per adapter that can call muxscale, keyed by
	// the path the agent POSTs to. The daemon speaks the normalized HookEvent;
	// each adapter owns its own payload and reply shape.
	codecs map[string]adapter.HookCodec
	// infos is each codec adapter's static description, for the fidelity a
	// hook-driven session can honestly claim.
	infos map[string]adapter.Info

	// peerPID finds the process behind a hook request; a field so tests can
	// stand in for lsof.
	peerPID func(remoteAddr string) (int, error)
	// linked holds the agent sessions already tied to their process, so the
	// lookup runs once per session rather than on every hook.
	linkMu sync.Mutex
	linked map[string]bool
}

// SetManager attaches the manager, enabling transcript tailing for
// hook-driven sessions. Optional so the hook server stays testable alone.
func (h *HookServer) SetManager(m *Manager) { h.mgr = m }

// NewHookServer builds the handler. holdTime is how long a decision hook waits
// for a human before giving up and falling through.
//
// adapters may be nil in tests that only exercise storage and auth; no codec
// means no hook routes, which is an honest empty daemon.
func NewHookServer(reg *Registry, adapters *adapter.Registry, token string, hold time.Duration, log *slog.Logger) *HookServer {
	h := &HookServer{reg: reg, token: token, holdTime: hold, log: log,
		peerPID: attach.PeerPID, linked: map[string]bool{},
		codecs: map[string]adapter.HookCodec{}, infos: map[string]adapter.Info{}}
	if adapters != nil {
		for _, a := range adapters.All() {
			if c, ok := a.(adapter.HookCodec); ok {
				h.codecs[c.HookPath()] = c
				h.infos[a.Info().ID] = a.Info()
			}
		}
	}
	return h
}

// Routes registers handlers on a mux.
func (h *HookServer) Routes(mux *http.ServeMux) {
	for path, codec := range h.codecs {
		path, codec := path, codec
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			h.handleHook(w, r, codec)
		})
	}
	mux.HandleFunc("/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	})
}

// noDecision is the fail-open body a codec returns for a transport that has
// nothing to decide. It is also every error path's answer.
func noDecision(codec adapter.HookCodec, e adapter.HookEvent) any {
	return codec.EncodeHookReply(e, nil)
}

func (h *HookServer) handleHook(w http.ResponseWriter, r *http.Request, codec adapter.HookCodec) {
	var empty adapter.HookEvent
	// Every early return is a 200 with the agent's own "no decision": daemon
	// down, bad token, malformed body, or an agent build we have not seen.
	if r.Method != http.MethodPost {
		writeJSON(w, noDecision(codec, empty))
		return
	}
	if !h.authorized(r) {
		h.log.Warn("hook request rejected: bad token", "remote", r.RemoteAddr, "path", r.URL.Path)
		writeJSON(w, noDecision(codec, empty))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		h.log.Warn("hook body read failed", "err", err)
		writeJSON(w, noDecision(codec, empty))
		return
	}
	e, err := codec.DecodeHook(body)
	if err != nil {
		h.log.Warn("hook payload decode failed", "path", r.URL.Path, "err", err)
		writeJSON(w, noDecision(codec, empty))
		return
	}

	pid := h.link(&e, r)
	sess := h.applyHook(&e)
	h.linkNew(pid, e.AgentID, sess.ID)

	// Decision hooks block here while a human is asked. Everything else has
	// already been recorded and returns immediately.
	switch e.Kind {
	case adapter.HookApproval, adapter.HookElicit:
		writeJSON(w, h.awaitDecision(r, sess, &e, codec))
	default:
		writeJSON(w, noDecision(codec, e))
	}
}

// link ties a hook's agent session to the process that sent it, so the row
// the process scan made for that process and the row the hooks make for the
// session are one row, not two.
//
// The two paths key on different things -- the scan on the process, the hooks
// on the agent's own session id -- and nothing else tied them reliably: the
// transcript path, which used to, can be claimed by whichever path saw it
// first and then never matches. Each showed as its own session, and the one
// with the hooks' exact state could not be typed into, while the one that
// could, from the scan, had no transcript.
//
// The scan's row wins a merge: it holds the terminal muxscale types into, and
// is the one a phone has been showing.
//
// It returns the process id it found, 0 when it did not look or could not
// tell.
func (h *HookServer) link(e *adapter.HookEvent, r *http.Request) int {
	if e.SessionID == "" {
		return 0
	}
	h.linkMu.Lock()
	done := h.linked[e.AgentID+"/"+e.SessionID]
	h.linked[e.AgentID+"/"+e.SessionID] = true
	h.linkMu.Unlock()
	if done {
		return 0
	}
	// A command-hook relay is a short-lived child of the agent, so its socket
	// peer is not the agent. It names the agent's pid in a header instead;
	// direct HTTP hooks (Claude Code) fall back to the socket peer.
	pid := 0
	if v := r.Header.Get("X-Muxscale-Peer-PID"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pid = n
		}
	}
	if pid == 0 {
		if h.peerPID == nil {
			return 0
		}
		var err error
		pid, err = h.peerPID(r.RemoteAddr)
		if err != nil {
			h.log.Debug("could not tie a hook to its process", "session", e.SessionID, "err", err)
			return 0
		}
	}
	procKey := "pid:" + strconv.Itoa(pid)
	hookID, hasHook := h.reg.IDFor(e.AgentID, e.SessionID)
	procID, hasProc := h.reg.IDFor(e.AgentID, procKey)
	switch {
	case hasProc && !hasHook:
		h.reg.Alias(e.AgentID, e.SessionID, procID)
	case hasProc && hasHook && procID != hookID:
		h.reg.Alias(e.AgentID, e.SessionID, procID)
		if h.mgr != nil {
			h.mgr.Merge(procID, hookID)
		} else {
			h.reg.Remove(hookID)
		}
		h.log.Info("merged a session found twice", "kept", procID, "dropped", hookID, "pid", pid)
	case hasHook && !hasProc:
		h.reg.Alias(e.AgentID, procKey, hookID)
	}
	return pid
}

// linkNew finishes link for a session the hook has only now created: there
// was no row to alias the process to until applyHook made one.
func (h *HookServer) linkNew(pid int, agentID, id string) {
	// The caller already aliased the agent's own session key; this covers the
	// process key for the first hook of a session.
	if pid == 0 {
		return
	}
	procKey := "pid:" + strconv.Itoa(pid)
	if _, ok := h.reg.IDFor(agentID, procKey); !ok {
		h.reg.Alias(agentID, procKey, id)
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
func (h *HookServer) applyHook(e *adapter.HookEvent) *protocol.Session {
	fidelity := protocol.FidelityFull
	if info, ok := h.infos[e.AgentID]; ok {
		fidelity = info.MaxFidelity()
	}
	sess := h.reg.Upsert(e.AgentID, e.SessionID, func(s *protocol.Session) {
		if e.Cwd != "" {
			s.Cwd = e.Cwd
		}
		if e.SessionFile != "" {
			s.TranscriptPath = e.SessionFile
		}
		if e.PromptID != "" {
			s.PromptID = e.PromptID
		}
		if e.Model != "" {
			s.Model = e.Model
		}
		if e.AgentVersion != "" {
			s.AgentVer = e.AgentVersion
		}
		s.Attachment = protocol.AttachNative
		s.Fidelity = fidelity
	})

	// An event payload usually names the transcript, so a hook-driven session
	// gets its full structured transcript with no path guessing at all.
	if e.SessionFile != "" && h.mgr != nil {
		h.mgr.EnsureTailer(sess.ID, e.SessionFile)
	}

	now := time.Now()
	guess := func(st protocol.SessionState, reason string) protocol.StateGuess {
		// Event-sourced state is authoritative: the agent told us, so there is
		// nothing to infer and confidence is exactly 1.
		return protocol.StateGuess{State: st, Confidence: 1.0, Reason: reason,
			Evidence: "hook:" + e.EventName, At: now}
	}

	switch e.Kind {
	case adapter.HookSessionStart:
		h.reg.SetState(sess.ID, guess(protocol.StateRunning, "session started"))

	case adapter.HookTurnStart:
		h.reg.SetState(sess.ID, guess(protocol.StateRunning, "agent started working"))

	case adapter.HookUserPrompt:
		h.reg.SetState(sess.ID, guess(protocol.StateRunning, "user submitted a prompt"))

	case adapter.HookToolCall:
		h.reg.AppendEvent(protocol.TranscriptEvent{
			SessionID: sess.ID, Kind: protocol.EventToolCall, At: now,
			ToolName: e.ToolName, ToolUseID: e.ToolUseID, ToolInput: e.ToolInput,
			IsSidechain: e.IsSidechain,
		})
		h.reg.SetState(sess.ID, guess(protocol.StateRunning, "running "+e.ToolName))

	case adapter.HookToolResult:
		h.reg.AppendEvent(protocol.TranscriptEvent{
			SessionID: sess.ID, Kind: protocol.EventToolResult, At: now,
			ToolName: e.ToolName, ToolUseID: e.ToolUseID,
			IsError: e.IsError, ToolOutput: e.ToolOutput,
			IsSidechain: e.IsSidechain,
		})

	case adapter.HookStop:
		// The single highest-value signal in the product: the turn is over and
		// the agent is waiting on a human.
		h.reg.SetState(sess.ID, guess(protocol.StateWaitingInput, "turn complete"))

	case adapter.HookIdle:
		h.reg.SetState(sess.ID, guess(protocol.StateWaitingInput, "agent idle"))

	case adapter.HookNotification:
		h.reg.SetState(sess.ID, guess(protocol.StateWaitingInput, "agent requested attention"))

	case adapter.HookSessionEnd:
		// SessionEnd has a short budget on every transport, so this path must
		// stay trivial. Record and return.
		h.reg.SetState(sess.ID, guess(protocol.StateExited, "session ended"))

	case adapter.HookCompaction:
		h.reg.AppendEvent(protocol.TranscriptEvent{
			SessionID: sess.ID, Kind: protocol.EventCompaction, At: now,
			Text: e.EventName,
		})

	case adapter.HookSubagentStart, adapter.HookSubagentStop:
		// Recorded for the nested-thread view; does not change parent state.

	default:
		// An event we do not handle yet. Log and move on -- agents add events
		// regularly and an unknown one is not an error.
		h.log.Debug("unhandled hook event", "agent", e.AgentID, "event", e.EventName)
	}

	return sess
}

// awaitDecision parks the request while a human is asked.
func (h *HookServer) awaitDecision(r *http.Request, sess *protocol.Session, e *adapter.HookEvent, codec adapter.HookCodec) any {
	req := &protocol.ApprovalRequest{
		ID:        newID(),
		SessionID: sess.ID,
		Source:    protocol.ApprovalNative,
		Kind:      "tool_use",
		ToolName:  e.ToolName,
		ToolInput: e.ToolInput,
		ToolUseID: e.ToolUseID,
		Subject:   e.Subject(),
		Options:   []string{"allow", "deny"},
		PromptID:  e.PromptID,
		AskedAt:   time.Now(),
		Deadline:  time.Now().Add(h.holdTime),
	}
	if e.Kind == adapter.HookElicit {
		req.Kind = "elicitation"
	}

	h.reg.SetState(sess.ID, protocol.StateGuess{
		State: protocol.StateWaitingApproval, Confidence: 1.0,
		Reason: "approval required: " + req.Subject, Evidence: "hook:" + e.EventName,
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
		return codec.EncodeHookReply(*e, &d)

	case <-timer.C:
		// Nobody answered. Return no decision so the agent asks locally. This
		// is the correct outcome, not a failure: the human at the keyboard is
		// still a human.
		h.reg.ResolveApproval(req.ID, protocol.ApprovalDecision{Behavior: "", Reason: "timeout"})
		h.clearApprovalState(sess.ID, "approval timed out; agent is asking locally")
		h.log.Info("approval timed out, falling through to local prompt", "session", sess.ID)
		return codec.EncodeHookReply(*e, nil)

	case <-r.Context().Done():
		// The agent hung up -- typically because the person at the keyboard
		// answered first. Withdraw the phone's pending approval.
		h.reg.ResolveApproval(req.ID, protocol.ApprovalDecision{Behavior: "", Reason: "answered at the keyboard"})
		h.clearApprovalState(sess.ID, "answered at the keyboard")
		h.log.Info("approval withdrawn: agent hung up", "session", sess.ID)
		return codec.EncodeHookReply(*e, nil)
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
