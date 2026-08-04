package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter/claudecode"
	"github.com/muxscale/muxscale/go/internal/attach"
	"github.com/muxscale/muxscale/go/internal/protocol"
	"github.com/muxscale/muxscale/go/internal/store"
	"github.com/muxscale/muxscale/go/internal/vt"
)

// Manager owns wrapped sessions and the discovery loop, and persists
// everything the registry sees.
type Manager struct {
	reg   *Registry
	store *store.Store
	log   *slog.Logger

	mu      sync.RWMutex
	wrapped map[string]*attach.Wrapped    // muxscale session id -> pty
	tailers map[string]*claudecode.Tailer // muxscale session id -> transcript tailer
	screens map[string]*vt.Emulator       // muxscale session id -> screen state

	// tmuxTargets maps a session to an adopted tmux pane, which is how an
	// already-running session becomes writable without changing anyone's habits.
	tmuxTargets map[string]string

	// screenMu guards emulator writes/reads separately from the map, so a
	// client reading a snapshot never blocks the PTY drain loop for long.
	screenMu sync.Mutex
}

// NewManager wires a registry to durable storage.
func NewManager(reg *Registry, st *store.Store, log *slog.Logger) *Manager {
	m := &Manager{
		reg: reg, store: st, log: log,
		wrapped:     map[string]*attach.Wrapped{},
		tailers:     map[string]*claudecode.Tailer{},
		screens:     map[string]*vt.Emulator{},
		tmuxTargets: map[string]string{},
	}
	go m.persistLoop()
	return m
}

// persistLoop mirrors registry changes into SQLite. It runs in its own
// goroutine so a slow disk cannot stall a hook handler that is holding an
// agent's request open.
func (m *Manager) persistLoop() {
	ch, cancel := m.reg.Subscribe()
	defer cancel()
	for c := range ch {
		switch c.Kind {
		case ChangeSessionUpsert, ChangeStateChange:
			if c.Session != nil {
				if err := m.store.SaveSession(c.Session); err != nil {
					m.log.Warn("persisting session failed", "err", err)
				}
			}
		case ChangeTranscript:
			if c.Event != nil {
				if err := m.store.AppendEvents([]protocol.TranscriptEvent{*c.Event}); err != nil {
					m.log.Warn("persisting transcript failed", "err", err)
				}
			}
		case ChangeApprovalAsk:
			if c.Approval != nil {
				_ = m.store.RecordApproval(c.Approval)
				_ = m.store.Audit("agent", "approval_requested", c.Approval.SessionID, c.Approval.Subject)
			}
		case ChangeApprovalResolve:
			if c.Approval != nil {
				_ = m.store.Audit("human", "approval_resolved", c.Approval.SessionID, c.Approval.Subject)
			}
		}
	}
}

// Restore reloads persisted sessions after a daemon restart.
//
// Anything that was running is marked DETACHED rather than shown as live: the
// daemon owned those PTYs and they did not survive. Claiming otherwise would
// put a dead session at the top of someone's phone.
func (m *Manager) Restore() (int, error) {
	sessions, err := m.store.LoadSessions()
	if err != nil {
		return 0, err
	}
	var restored int
	for i := range sessions {
		s := sessions[i]
		if s.State.Terminal() {
			continue // keep terminal sessions in the DB, not in the live list
		}
		state := protocol.StateDetached
		reason := "daemon restarted; attachment lost"
		if s.Attachment == protocol.AttachNative {
			// Hook-driven sessions re-announce themselves on their next event,
			// so they are recoverable rather than lost.
			state = protocol.StateUnknown
			reason = "daemon restarted; awaiting the next hook"
		}
		// Insert under the persisted ID so notification threads and any saved
		// deep links keep pointing at the same session across restarts.
		s.State = state
		s.StateSince = time.Now()
		s.Guess = protocol.StateGuess{
			State: state, Confidence: 1.0, Reason: reason,
			Evidence: "restore", At: time.Now(),
		}
		s.PendingApproval = nil // in-flight approvals did not survive
		m.reg.Insert(s)
		restored++
	}
	return restored, nil
}

// RunOptions launches an agent under a daemon-owned PTY.
type RunOptions struct {
	Command []string
	Dir     string
	Name    string
	AgentID string
	Cols    uint16
	Rows    uint16
}

// Run starts a wrapped session.
func (m *Manager) Run(opt RunOptions) (*protocol.Session, error) {
	if opt.Dir == "" {
		opt.Dir, _ = os.Getwd()
	}
	if opt.AgentID == "" {
		opt.AgentID = attach.AgentBinaries[filepath.Base(opt.Command[0])]
		if opt.AgentID == "" {
			opt.AgentID = "generic-tui"
		}
	}

	w, err := attach.Wrap(attach.WrapOptions{
		Command: opt.Command, Dir: opt.Dir, Cols: opt.Cols, Rows: opt.Rows,
	})
	if err != nil {
		return nil, err
	}

	natural := "pid:" + strconv.Itoa(w.Pid())
	sess := m.reg.Upsert(opt.AgentID, natural, func(s *protocol.Session) {
		s.Cwd = opt.Dir
		s.Title = opt.Name
		s.Attachment = protocol.AttachWrap
		s.Fidelity = protocol.FidelityFull
	})

	m.mu.Lock()
	m.wrapped[sess.ID] = w
	m.mu.Unlock()

	m.reg.SetState(sess.ID, protocol.StateGuess{
		State: protocol.StateRunning, Confidence: 1.0,
		Reason: "wrapped session started", At: time.Now(),
	})
	_ = m.store.Audit("cli", "session_started", sess.ID, fmt.Sprint(opt.Command))

	go m.superviseWrapped(sess.ID, w)
	return sess, nil
}

// superviseWrapped feeds output into the VT emulator and records the exit.
func (m *Manager) superviseWrapped(sessionID string, w *attach.Wrapped) {
	// Draining is mandatory regardless of whether anyone is watching: an unread
	// PTY fills its buffer and blocks the agent, which would look like a hang
	// caused by muxscale.
	em := vt.NewEmulator(int(w.Cols), int(w.Rows))
	m.mu.Lock()
	m.screens[sessionID] = em
	m.mu.Unlock()

	buf := make([]byte, 32*1024)
	for {
		n, err := w.Read(buf)
		if n > 0 {
			m.screenMu.Lock()
			_, _ = em.Write(buf[:n])
			m.screenMu.Unlock()
			m.reg.NoteOutput(sessionID, n)
		}
		if err != nil {
			break
		}
	}
	<-w.Exited()

	code := w.ExitCode()
	state := protocol.StateExited
	reason := fmt.Sprintf("exited with code %d", code)
	if code < 0 {
		state = protocol.StateCrashed
		reason = "process terminated abnormally"
	}
	m.reg.SetState(sessionID, protocol.StateGuess{
		State: state, Confidence: 1.0, Reason: reason,
		Evidence: "pty:exit", At: time.Now(),
	})

	m.mu.Lock()
	delete(m.wrapped, sessionID)
	m.mu.Unlock()
	_ = m.store.Audit("agent", "session_exited", sessionID, reason)
}

// Screen returns a session's current screen snapshot, if it has one.
func (m *Manager) Screen(sessionID string) (vt.Snapshot, bool) {
	m.mu.RLock()
	em := m.screens[sessionID]
	m.mu.RUnlock()
	if em == nil {
		return vt.Snapshot{}, false
	}
	m.screenMu.Lock()
	defer m.screenMu.Unlock()
	return em.Grid().Snapshot(), true
}

// ScreenText returns the session's screen as plain text, for the CLI.
func (m *Manager) ScreenText(sessionID string) (string, bool) {
	m.mu.RLock()
	em := m.screens[sessionID]
	m.mu.RUnlock()
	if em == nil {
		return "", false
	}
	m.screenMu.Lock()
	defer m.screenMu.Unlock()
	return em.Grid().Text(), true
}

// SendInput delivers input to a session, honouring its fence.
func (m *Manager) SendInput(sessionID string, text string, key attach.NamedKey, submit bool, fence attach.Fence, actor string) error {
	sess, ok := m.reg.Get(sessionID)
	if !ok {
		return fmt.Errorf("unknown session %s", sessionID)
	}

	// Fence first: a stale reply must never reach the agent, and the check has
	// to happen against live state, not the state the client saw.
	if err := fence.Check(sess.PromptID, string(sess.State)); err != nil {
		_ = m.store.Audit(actor, "input_blocked_by_fence", sessionID, err.Error())
		return err
	}

	var payload []byte
	if key != "" {
		b, err := attach.EncodeKey(key)
		if err != nil {
			return err
		}
		payload = b
	} else {
		payload = attach.EncodeText(text, submit)
	}

	m.mu.RLock()
	w := m.wrapped[sessionID]
	target := m.tmuxTargets[sessionID]
	m.mu.RUnlock()

	switch {
	case w != nil:
		if _, err := w.Write(payload); err != nil {
			return err
		}
	case target != "":
		if err := attach.SendKeys(target, payload); err != nil {
			return err
		}
	default:
		// Say exactly why and exactly how to fix it. A bare "read-only" tells
		// the user nothing they can act on.
		return fmt.Errorf(
			"session %s is read-only: muxscale does not own its terminal.\n"+
				"To enable replies, either restart it under `muxscale run`, "+
				"run it inside tmux, or enable remote control in your terminal "+
				"(kitty, WezTerm, or iTerm2)", sessionID)
	}
	// Record that input happened and how much, never the content by default.
	_ = m.store.Audit(actor, "input_sent", sessionID,
		fmt.Sprintf("key=%s bytes=%d submit=%v", key, len(payload), submit))
	return nil
}

// Kill terminates a wrapped session.
func (m *Manager) Kill(sessionID string) error {
	m.mu.RLock()
	w := m.wrapped[sessionID]
	m.mu.RUnlock()
	if w == nil {
		return fmt.Errorf("session %s is not a wrapped session", sessionID)
	}
	_ = m.store.Audit("human", "session_killed", sessionID, "")
	return w.Close(5 * time.Second)
}

// DiscoverLoop periodically scans for agent processes muxscale did not launch.
func (m *Manager) DiscoverLoop(stop <-chan struct{}, interval time.Duration) {
	if interval == 0 {
		interval = 3 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	m.scanOnce()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.scanOnce()
		}
	}
}

func (m *Manager) scanOnce() {
	m.scanTmux()

	procs, err := attach.Discover()
	if err != nil {
		m.log.Debug("discovery failed", "err", err)
		return
	}
	for _, p := range procs {
		agentID := attach.AgentBinaries[p.Command]
		if agentID == "" {
			continue
		}
		// A wrapped session already has a higher-fidelity attachment under a
		// pid natural key; do not shadow it with a discovery-only entry.
		if m.hasWrappedPid(p.Pid) {
			continue
		}
		natural := "pid:" + strconv.Itoa(p.Pid)
		sess := m.reg.Upsert(agentID, natural, func(s *protocol.Session) {
			if s.Attachment == protocol.AttachWrap || s.Attachment == protocol.AttachNative {
				return // a better attachment already owns this session
			}
			s.Cwd = p.Cwd
			s.Attachment = protocol.AttachTranscript
			s.Fidelity = protocol.FidelityReadOnly
			if via := p.WritableVia(); via != "" {
				// Reachable for input, but not yet attached that way.
				s.Fidelity = protocol.FidelityStructured
			}
			if s.CreatedAt.IsZero() && !p.Started.IsZero() {
				s.CreatedAt = p.Started
			}
		})
		// Discovered Claude Code sessions get their transcript followed, which
		// is what turns a bare process listing into a readable session.
		if agentID == "claude-code" {
			if s, ok := m.reg.Get(sess.ID); ok {
				path := s.TranscriptPath
				if path == "" && p.Cwd != "" {
					if files, err := claudecode.FindTranscripts(p.Cwd); err == nil && len(files) > 0 {
						path = files[0]
						m.reg.Upsert(agentID, natural, func(t *protocol.Session) {
							t.TranscriptPath = path
						})
					}
				}
				m.EnsureTailer(sess.ID, path)
			}
		}

		if s, ok := m.reg.Get(sess.ID); ok && s.State == protocol.StateStarting {
			m.reg.SetState(sess.ID, protocol.StateGuess{
				State: protocol.StateUnknown, Confidence: 0.5,
				Reason:   "discovered by process scan; no hook or transcript yet",
				Evidence: "scan:pid=" + strconv.Itoa(p.Pid), At: time.Now(),
			})
		}
	}
}

// EnsureTailer starts following a session's transcript file if it is not
// already being followed. Idempotent, so the discovery loop can call it freely.
func (m *Manager) EnsureTailer(sessionID, path string) {
	if path == "" {
		return
	}
	m.mu.Lock()
	if _, running := m.tailers[sessionID]; running {
		m.mu.Unlock()
		return
	}
	tl := claudecode.NewTailer(path, 750*time.Millisecond)
	m.tailers[sessionID] = tl
	m.mu.Unlock()

	// Resume from what the store already has rather than re-reading a file
	// that can be hundreds of megabytes.
	if maxSeq, err := m.store.MaxSeq(sessionID); err == nil && maxSeq > 0 {
		if err := tl.SeekEnd(maxSeq); err != nil {
			m.log.Debug("tailer seek failed", "session", sessionID, "err", err)
		}
	}
	go m.tailLoop(sessionID, tl)
}

func (m *Manager) tailLoop(sessionID string, tl *claudecode.Tailer) {
	t := time.NewTicker(tl.Interval())
	defer t.Stop()
	defer func() {
		m.mu.Lock()
		delete(m.tailers, sessionID)
		m.mu.Unlock()
	}()

	var misses int
	for range t.C {
		if s, ok := m.reg.Get(sessionID); ok && s.State.Terminal() {
			return
		}
		events, meta, err := tl.Poll()
		if err != nil {
			misses++
			if misses > 20 { // file gone for ~15s; stop following it
				return
			}
			continue
		}
		misses = 0
		if len(events) == 0 {
			continue
		}

		for i := range events {
			events[i].SessionID = sessionID
			m.reg.AppendEvent(events[i])
		}

		// Metadata recovered from the transcript is free enrichment: title,
		// branch, and model without asking the agent anything.
		m.reg.Upsert("claude-code", naturalOf(m.reg, sessionID), func(s *protocol.Session) {
			if meta.Title != "" {
				s.Title = meta.Title
			}
			if meta.GitBranch != "" {
				s.GitBranch = meta.GitBranch
			}
			if meta.Model != "" {
				s.Model = meta.Model
			}
			if meta.Version != "" {
				s.AgentVer = meta.Version
			}
		})

		// Only infer state for sessions with no authoritative hook source: a
		// guess must never override what the agent told us directly.
		if s, ok := m.reg.Get(sessionID); ok && s.Attachment != protocol.AttachNative {
			if fi, err := os.Stat(tl.Path()); err == nil {
				guess := claudecode.InferState(events, fi.ModTime(), 6*time.Second)
				m.reg.SetState(sessionID, guess)
			}
		}
	}
}

// naturalOf looks up a session's natural key so Upsert targets the right row.
func naturalOf(reg *Registry, sessionID string) string {
	if s, ok := reg.Get(sessionID); ok {
		return s.NaturalKey
	}
	return sessionID
}

// scanTmux adopts agent panes so already-running sessions accept input.
func (m *Manager) scanTmux() {
	if !attach.TmuxAvailable() {
		return
	}
	panes, err := attach.FindAgentPanes()
	if err != nil {
		m.log.Debug("tmux scan failed", "err", err)
		return
	}
	for _, p := range panes {
		agentID := attach.AgentBinaries[p.Command]
		if agentID == "" {
			continue
		}
		// Key on pane_id (%N): it survives window renumbering, whereas
		// session:window.pane does not.
		natural := "tmux:" + p.PaneID
		sess := m.reg.Upsert(agentID, natural, func(s *protocol.Session) {
			s.Cwd = p.Path
			s.Attachment = protocol.AttachTmux
			s.Fidelity = protocol.FidelityFull
		})

		m.mu.Lock()
		m.tmuxTargets[sess.ID] = p.PaneID
		m.mu.Unlock()

		if s, ok := m.reg.Get(sess.ID); ok && s.State == protocol.StateStarting {
			m.reg.SetState(sess.ID, protocol.StateGuess{
				State: protocol.StateUnknown, Confidence: 0.5,
				Reason:   "adopted from tmux; no hook or transcript yet",
				Evidence: "tmux:" + p.PaneID, At: time.Now(),
			})
		}
		if agentID == "claude-code" && p.Path != "" {
			if files, err := claudecode.FindTranscripts(p.Path); err == nil && len(files) > 0 {
				m.reg.Upsert(agentID, natural, func(t *protocol.Session) {
					t.TranscriptPath = files[0]
				})
				m.EnsureTailer(sess.ID, files[0])
			}
		}
	}
}

func (m *Manager) hasWrappedPid(pid int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, w := range m.wrapped {
		if w.Pid() == pid {
			return true
		}
	}
	return false
}

// Shutdown closes wrapped sessions.
func (m *Manager) Shutdown(grace time.Duration) {
	m.mu.Lock()
	ws := make([]*attach.Wrapped, 0, len(m.wrapped))
	for _, w := range m.wrapped {
		ws = append(ws, w)
	}
	m.mu.Unlock()
	for _, w := range ws {
		_ = w.Close(grace)
	}
}
