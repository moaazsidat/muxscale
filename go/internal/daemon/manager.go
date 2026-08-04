package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/muxscale/muxscale/go/internal/attach"
	"github.com/muxscale/muxscale/go/internal/protocol"
	"github.com/muxscale/muxscale/go/internal/store"
)

// Manager owns wrapped sessions and the discovery loop, and persists
// everything the registry sees.
type Manager struct {
	reg   *Registry
	store *store.Store
	log   *slog.Logger

	mu      sync.RWMutex
	wrapped map[string]*attach.Wrapped // muxscale session id -> pty
}

// NewManager wires a registry to durable storage.
func NewManager(reg *Registry, st *store.Store, log *slog.Logger) *Manager {
	m := &Manager{reg: reg, store: st, log: log, wrapped: map[string]*attach.Wrapped{}}
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

// superviseWrapped drains output and records the exit.
func (m *Manager) superviseWrapped(sessionID string, w *attach.Wrapped) {
	// M0/M1: output is drained but not yet fed to a VT emulator. Draining is
	// mandatory regardless -- an unread PTY fills its buffer and blocks the
	// agent, which would look like a hang caused by muxscale.
	buf := make([]byte, 32*1024)
	for {
		n, err := w.Read(buf)
		if n > 0 {
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

	m.mu.RLock()
	w := m.wrapped[sessionID]
	m.mu.RUnlock()
	if w == nil {
		return fmt.Errorf(
			"session %s is read-only: muxscale does not own its terminal.\n"+
				"Restart it under `muxscale run`, or move it into tmux, to enable replies", sessionID)
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

	if _, err := w.Write(payload); err != nil {
		return err
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
		if s, ok := m.reg.Get(sess.ID); ok && s.State == protocol.StateStarting {
			m.reg.SetState(sess.ID, protocol.StateGuess{
				State: protocol.StateUnknown, Confidence: 0.5,
				Reason:   "discovered by process scan; no hook or transcript yet",
				Evidence: "scan:pid=" + strconv.Itoa(p.Pid), At: time.Now(),
			})
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
