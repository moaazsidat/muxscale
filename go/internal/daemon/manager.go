package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/adapter/claudecode"
	"github.com/muxscale/muxscale/go/internal/attach"
	"github.com/muxscale/muxscale/go/internal/protocol"
	"github.com/muxscale/muxscale/go/internal/store"
	"github.com/muxscale/muxscale/go/internal/vt"
)

// Manager owns wrapped sessions and the discovery loop, and persists
// everything the registry sees.
type Manager struct {
	reg      *Registry
	adapters *adapter.Registry
	store    *store.Store
	log      *slog.Logger

	mu      sync.RWMutex
	wrapped map[string]*attach.Wrapped    // muxscale session id -> pty
	tailers map[string]*claudecode.Tailer // muxscale session id -> transcript tailer
	screens map[string]*vt.Emulator       // muxscale session id -> screen state

	// closing records sessions a human asked to end, so their exit-by-signal is
	// reported as "closed" rather than "crashed".
	closing map[string]bool

	// tmuxTargets maps a session to an adopted tmux pane, which is how an
	// already-running session becomes writable without changing anyone's habits.
	tmuxTargets map[string]string

	// screenMu guards emulator writes/reads separately from the map, so a
	// client reading a snapshot never blocks the PTY drain loop for long.
	screenMu sync.Mutex
}

// NewManager wires a registry to durable storage.
//
// adapters may be nil, in which case agent identification falls back to the
// binary-name table alone. Tests that do not care about adapter dispatch pass
// nil rather than constructing the whole built-in set.
func NewManager(reg *Registry, adapters *adapter.Registry, st *store.Store, log *slog.Logger) *Manager {
	m := &Manager{
		reg: reg, adapters: adapters, store: st, log: log,
		wrapped:     map[string]*attach.Wrapped{},
		tailers:     map[string]*claudecode.Tailer{},
		screens:     map[string]*vt.Emulator{},
		closing:     map[string]bool{},
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
		opt.AgentID = m.identify(filepath.Base(opt.Command[0]), opt.Command)
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

// ShellAgentID marks a session that is a plain shell rather than an agent.
// Adapters never claim it, so nothing tries to parse a transcript out of it.
const ShellAgentID = "shell"

// RunShell opens a login shell in an agent session's working directory.
//
// This is the "drop me into a terminal in this project" affordance. The cwd is
// read from the parent session rather than taken from the caller: a client that
// could name any directory would turn one authenticated endpoint into arbitrary
// filesystem access, and the whole point is that it lands where the agent is.
//
// One shell per parent. A second call returns the existing one while it is
// alive, so navigating away and back does not strand a process or lose
// scrollback.
func (m *Manager) RunShell(parentID string, cols, rows uint16) (*protocol.Session, error) {
	parent, ok := m.reg.Get(parentID)
	if !ok {
		return nil, fmt.Errorf("unknown session %s", parentID)
	}
	if parent.Cwd == "" {
		return nil, fmt.Errorf("session %s has no known working directory", parentID)
	}

	if existing, ok := m.liveShellFor(parentID); ok {
		return existing, nil
	}

	sess, err := m.Run(RunOptions{
		Command: []string{loginShell()},
		Dir:     parent.Cwd,
		Name:    "shell — " + filepath.Base(parent.Cwd),
		AgentID: ShellAgentID,
		Cols:    cols, Rows: rows,
	})
	if err != nil {
		return nil, err
	}

	// Set after Run so the fields survive the Upsert inside it.
	updated := m.reg.Update(sess.ID, func(s *protocol.Session) {
		s.ParentID = parentID
		// The client that asked for this shell is its only viewer, which is the
		// condition §5.5 attaches to remote resize.
		s.AllowRemoteResize = true
	})
	if updated != nil {
		return updated, nil
	}
	return sess, nil
}

// liveShellFor returns the running shell belonging to a parent session.
func (m *Manager) liveShellFor(parentID string) (*protocol.Session, bool) {
	for _, s := range m.reg.List() {
		if s.ParentID != parentID || s.AgentID != ShellAgentID || s.State.Terminal() {
			continue
		}
		m.mu.RLock()
		_, alive := m.wrapped[s.ID]
		m.mu.RUnlock()
		if alive {
			found := s
			return &found, true
		}
	}
	return nil, false
}

// loginShell picks the shell to open. $SHELL is what the user chose; the
// fallback is only for a daemon started by a service manager with a bare
// environment.
func loginShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// Resize changes a session's PTY dimensions.
//
// Refused unless the session opted in. Spec §5.5: a viewer resizing a shared
// PTY reflows the TUI for everyone attached to it, including whoever is sat at
// the physical keyboard, and some TUIs lose scrollback when that happens. Only
// sessions a remote client created for itself carry the flag.
func (m *Manager) Resize(sessionID string, cols, rows uint16) error {
	sess, ok := m.reg.Get(sessionID)
	if !ok {
		return fmt.Errorf("unknown session %s", sessionID)
	}
	if !sess.AllowRemoteResize {
		return fmt.Errorf("session %s does not permit remote resize: it has viewers "+
			"whose display would reflow (see spec §5.5)", sessionID)
	}
	if cols == 0 || rows == 0 {
		return fmt.Errorf("resize needs non-zero cols and rows, got %dx%d", cols, rows)
	}

	m.mu.RLock()
	w := m.wrapped[sessionID]
	em := m.screens[sessionID]
	m.mu.RUnlock()
	if w == nil {
		return fmt.Errorf("session %s has no PTY to resize", sessionID)
	}
	if err := w.Resize(cols, rows); err != nil {
		return err
	}
	// The emulator has to follow the PTY or every subsequent frame is decoded
	// against the wrong width.
	if em != nil {
		m.screenMu.Lock()
		em.Resize(int(cols), int(rows))
		m.screenMu.Unlock()
	}
	return nil
}

// ScreenClone returns an independent copy of a session's grid.
//
// The stream handler diffs against its own copy, so it must not share memory
// with the emulator the PTY drain loop is writing into.
func (m *Manager) ScreenClone(sessionID string) (*vt.Grid, bool) {
	m.mu.RLock()
	em := m.screens[sessionID]
	m.mu.RUnlock()
	if em == nil {
		return nil, false
	}
	m.screenMu.Lock()
	defer m.screenMu.Unlock()
	return em.Grid().Clone(), true
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

	m.mu.Lock()
	asked := m.closing[sessionID]
	delete(m.closing, sessionID)
	m.mu.Unlock()

	code := w.ExitCode()
	state := protocol.StateExited
	reason := fmt.Sprintf("exited with code %d", code)
	switch {
	case asked:
		// Someone closed this session. It exits by signal -- SIGHUP for a shell,
		// which reports as a negative code -- and calling that a crash would put
		// a red "crashed" badge on every terminal tab the user shut themselves.
		reason = "closed"
	case code < 0:
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
	m.mu.Lock()
	m.closing[sessionID] = true
	m.mu.Unlock()
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
		agentID := m.identify(p.Command, p.Args)
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

// Reconcile drops weaker duplicates of a session.
//
// The same real agent session is found by two independent paths that key on
// different things: process scan keys on pid, hooks key on the agent's own
// session id. Both are correct, and without this they produce two rows for one
// agent. The richer attachment wins, because it can do strictly more.
func (m *Manager) Reconcile(keepID, transcriptPath string) {
	if transcriptPath == "" {
		return
	}
	keep, ok := m.reg.Get(keepID)
	if !ok {
		return
	}
	for _, other := range m.reg.FindByTranscript(transcriptPath) {
		if other.ID == keepID {
			continue
		}
		if fidelityRank(other.Fidelity) >= fidelityRank(keep.Fidelity) {
			continue // the other one is at least as capable; leave it alone
		}
		m.log.Debug("dropping a duplicate session",
			"kept", keepID, "dropped", other.ID, "transcript", transcriptPath)
		m.reg.Remove(other.ID)

		m.mu.Lock()
		delete(m.tailers, other.ID)
		m.mu.Unlock()
	}
}

// fidelityRank orders attachments by capability, so reconciliation keeps the
// one that can do more rather than whichever was seen first.
func fidelityRank(f protocol.Fidelity) int {
	switch f {
	case protocol.FidelityNativeFull:
		return 4
	case protocol.FidelityFull:
		return 3
	case protocol.FidelityStructured:
		return 2
	case protocol.FidelityReadOnly:
		return 1
	}
	return 0
}

// EnsureTailer starts following a session's transcript file if it is not
// already being followed. Idempotent, so the discovery loop can call it freely.
func (m *Manager) EnsureTailer(sessionID, path string) {
	if path == "" {
		return
	}
	// A hook-driven session and a scan-discovered one can both be following the
	// same file. Collapse them before either starts streaming duplicates.
	m.Reconcile(sessionID, path)
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
		agentID := m.identify(p.Command, nil)
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

// identify resolves which agent a process belongs to.
//
// The adapter registry is asked first, because an adapter's Detect knows things
// the binary-name table cannot -- notably that a `node` process running
// @anthropic-ai/claude-code is Claude Code. The table remains the fallback for
// agents muxscale can see but has no adapter for yet (amp, cursor-agent, aider,
// goose, gemini): per spec §4.3.7 the process scan alone still gives their
// session list, and dropping them for lack of an adapter would be a regression.
//
// An ambiguous match is treated as no match. Two adapters claiming one process
// means their detect rules overlap, and attributing the session to whichever
// was checked first would be wrong intermittently and unreproducibly -- far
// worse than the session appearing under the generic table or not at all.
func (m *Manager) identify(command string, argv []string) string {
	if m.adapters != nil {
		if len(argv) == 0 {
			// tmux reports pane_current_command with no argv.
			argv = []string{command}
		}
		a, err := m.adapters.Match(adapter.Process{Argv: argv})
		if err != nil {
			m.log.Warn("ambiguous agent detection; falling back to the binary table",
				"command", command, "err", err)
		} else if a != nil {
			return a.Info().ID
		}
	}
	return attach.AgentBinaries[command]
}
