package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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

	mu sync.RWMutex
	// ingestMu orders storing a tailer's events against clearing them for a
	// rebuild.
	ingestMu sync.Mutex
	wrapped  map[string]*attach.Wrapped    // muxscale session id -> pty
	tailers  map[string]*claudecode.Tailer // muxscale session id -> transcript tailer
	screens  map[string]*vt.Emulator       // muxscale session id -> screen state

	// sampled is guarded by screenMu, not mu. See sampleScreen.
	//
	// sampled holds a screen reconstructed from an adopted terminal's own
	// idea of its contents, for sessions muxscale has no pty for. Kept
	// between polls rather than rebuilt each time so the grid-diffing that
	// drives the stream still produces small deltas.
	sampled map[string]*vt.Emulator

	// closing records sessions a human asked to end, so their exit-by-signal is
	// reported as "closed" rather than "crashed".
	closing map[string]bool

	// tmuxTargets maps a session to an adopted tmux pane, which is how an
	// already-running session becomes writable without changing anyone's habits.
	tmuxTargets map[string]string

	// termctlTargets maps a session to a terminal-emulator pane adopted through
	// that emulator's scripting interface. This is the last resort and the one
	// that covers the common case: an agent already running in an ordinary
	// terminal window, started long before the daemon, in no multiplexer.
	termctlTargets map[string]attach.TermTarget

	// termctlWarnOnce keeps a standing failure -- almost always a denied
	// Automation permission -- from being logged on every scan.
	termctlWarnOnce sync.Once

	// screenMu guards emulator writes/reads separately from the map, so a
	// client reading a snapshot never blocks the PTY drain loop for long.
	screenMu sync.Mutex

	// scanMu serializes discovery passes. The loop runs one every few seconds
	// and `muxscale status` runs one on demand; two at once would race to adopt
	// the same pane and could double-match a transcript.
	scanMu sync.Mutex
}

// NewManager wires a registry to durable storage.
//
// adapters may be nil, in which case agent identification falls back to the
// binary-name table alone. Tests that do not care about adapter dispatch pass
// nil rather than constructing the whole built-in set.
func NewManager(reg *Registry, adapters *adapter.Registry, st *store.Store, log *slog.Logger) *Manager {
	m := &Manager{
		reg: reg, adapters: adapters, store: st, log: log,
		wrapped:        map[string]*attach.Wrapped{},
		tailers:        map[string]*claudecode.Tailer{},
		screens:        map[string]*vt.Emulator{},
		closing:        map[string]bool{},
		tmuxTargets:    map[string]string{},
		termctlTargets: map[string]attach.TermTarget{},
		sampled:        map[string]*vt.Emulator{},
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
		case ChangeSessionRemove:
			if c.Session != nil {
				if err := m.store.DeleteSession(c.Session.ID); err != nil {
					m.log.Warn("forgetting a merged session failed", "err", err)
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

// staleHookSession is how long a session known only from its hooks can go
// unheard from and still be restored as live; past it, it is restored ended. Nothing else can say it ended:
// a process the scan found is checked against the process table, but a
// hook-only session that exits without a SessionEnd -- a crash, a test, a
// machine that slept through it -- otherwise stayed in the list forever.
const staleHookSession = 24 * time.Hour

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
		confidence := 1.0
		if s.Attachment == protocol.AttachNative && time.Since(s.LastEventAt) < staleHookSession {
			// Hook-driven sessions re-announce themselves on their next event,
			// so they are recoverable rather than lost. Until then this is a
			// guess, and says so: a certain "unknown" shut out the transcript
			// inference that would have found an idle session waiting, and
			// it read as unknown until someone next prompted it.
			state = protocol.StateUnknown
			reason = "daemon restarted; awaiting the next hook"
			confidence = 0.3
		}
		// Nothing can find this one again -- no process, no tmux pane -- and
		// nothing has been heard from it in a day: it ended, unannounced.
		refindable := strings.HasPrefix(s.NaturalKey, "pid:") || strings.HasPrefix(s.NaturalKey, "tmux:")
		if !refindable && time.Since(s.LastEventAt) >= staleHookSession {
			state = protocol.StateExited
			reason = "not heard from in over a day"
		}
		// Insert under the persisted ID so notification threads and any saved
		// deep links keep pointing at the same session across restarts.
		s.State = state
		s.StateSince = time.Now()
		s.Guess = protocol.StateGuess{
			State: state, Confidence: confidence, Reason: reason,
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
		return m.sampleScreen(sessionID)
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
	cols, rows := w.Size()
	em := vt.NewEmulator(int(cols), int(rows))
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
		g, ok := m.sampleScreen(sessionID)
		if !ok {
			return vt.Snapshot{}, false
		}
		return g.Snapshot(), true
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
		// No pty of our own. An adopted terminal pane can still be read, by
		// asking its emulator what it is showing.
		g, ok := m.sampleScreen(sessionID)
		if !ok {
			return "", false
		}
		return g.Text(), true
	}
	m.screenMu.Lock()
	defer m.screenMu.Unlock()
	return em.Grid().Text(), true
}

// sampleScreen reconstructs an adopted session's screen from what its terminal
// emulator reports.
//
// This is a sample, not a stream: it is whatever the window looked like at the
// moment it was asked, with no colour, because the emulator's scripting
// interface hands back plain text. It exists so the same grid endpoints work
// for a session muxscale merely adopted as for one it owns -- a client should
// not need a second code path to look at a terminal -- and clients are told
// which they have through the session's attachment. See attach.SampledNotice.
func (m *Manager) sampleScreen(sessionID string) (*vt.Grid, bool) {
	m.mu.RLock()
	term, ok := m.termctlTargets[sessionID]
	m.mu.RUnlock()
	if !ok {
		return nil, false
	}
	text, err := term.Text()
	if err != nil {
		if attach.IsTargetStale(err) {
			m.mu.Lock()
			delete(m.termctlTargets, sessionID)
			m.mu.Unlock()
			m.screenMu.Lock()
			delete(m.sampled, sessionID)
			m.screenMu.Unlock()
		}
		m.log.Debug("sampling an adopted terminal failed", "session", sessionID, "err", err)
		return nil, false
	}

	cols, rows := textDimensions(text)

	// sampled is guarded by screenMu rather than mu, so that the emulator and
	// the map holding it are under one lock. Two locks here was a nesting
	// order nothing else in this file uses, and the first bug in it left mu
	// held by a panicking request and wedged every later one.
	m.screenMu.Lock()
	defer m.screenMu.Unlock()
	em := m.sampled[sessionID]
	if em == nil {
		em = vt.NewEmulator(cols, rows)
		m.sampled[sessionID] = em
	}

	em.Resize(cols, rows)
	// Home and erase first: this is a whole-screen replacement, and without it
	// a shorter frame would leave the previous one'"'"'s tail on screen.
	_, _ = em.Write([]byte("\x1b[H\x1b[2J"))
	_, _ = em.Write([]byte(strings.ReplaceAll(text, "\n", "\r\n")))
	return em.Grid().Clone(), true
}

// textDimensions sizes a grid to hold a block of text.
//
// Width is counted in runes, not bytes, or a screen with any non-ASCII on it
// would be sized too wide and render with a ragged right edge.
func textDimensions(text string) (cols, rows int) {
	for _, line := range strings.Split(text, "\n") {
		if n := utf8.RuneCountInString(strings.TrimRight(line, "\r")); n > cols {
			cols = n
		}
		rows++
	}
	return clamp(cols, 20, 400), clamp(rows, 5, 200)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
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
		payload = attach.EncodeText(text, false)
	}

	if err := m.deliver(sessionID, payload); err != nil {
		return err
	}
	// The carriage return goes in a write of its own, after a pause. Agent
	// TUIs (Claude Code among them) treat a burst of bytes arriving at once as
	// a paste, and a return inside a paste is a newline in the composer, not a
	// submit -- so text+"\r" in one write left replies sitting in the box.
	if key == "" && submit {
		time.Sleep(submitDelay)
		if err := m.deliver(sessionID, []byte("\r")); err != nil {
			return err
		}
	}
	// Record that input happened and how much, never the content by default.
	_ = m.store.Audit(actor, "input_sent", sessionID,
		fmt.Sprintf("key=%s bytes=%d submit=%v", key, len(payload), submit))
	return nil
}

// submitDelay separates typed text from the return that submits it, long
// enough that the TUI has finished handling the text as its own input.
const submitDelay = 150 * time.Millisecond

// deliver writes raw bytes to whatever terminal the session is reachable by.
func (m *Manager) deliver(sessionID string, payload []byte) error {
	m.mu.RLock()
	w := m.wrapped[sessionID]
	target := m.tmuxTargets[sessionID]
	term, viaTerm := m.termctlTargets[sessionID]
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
	case viaTerm:
		if err := term.SendText(payload); err != nil {
			// The target is gone or has moved. Drop it so the next scan can
			// re-adopt rather than leaving a target that fails forever.
			if attach.IsTargetStale(err) {
				m.mu.Lock()
				delete(m.termctlTargets, sessionID)
				m.mu.Unlock()
			}
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

// AdoptReport describes what a discovery pass found and what it can reach.
type AdoptReport struct {
	Sessions  []AdoptRow              `json:"sessions"`
	Terminals []attach.TermCapability `json:"terminals"`
	Note      string                  `json:"note,omitempty"`
}

// AdoptRow is one session as the adoption pass left it.
type AdoptRow struct {
	ID             string              `json:"id"`
	AgentID        string              `json:"agent_id"`
	Title          string              `json:"title,omitempty"`
	Cwd            string              `json:"cwd,omitempty"`
	Attachment     protocol.Attachment `json:"attachment"`
	Fidelity       protocol.Fidelity   `json:"fidelity"`
	Writable       bool                `json:"writable"`
	TranscriptPath string              `json:"transcript_path,omitempty"`
}

// AdoptNow runs a discovery pass immediately and reports the result.
//
// The loop already runs every few seconds, so this is not how sessions get
// adopted -- it is how a user finds out what happened after they changed
// something (enabled terminal remote control, installed the shims, started the
// daemon) without waiting for the next tick and guessing. The report says both
// what muxscale reached and what it could reach if a setting were changed,
// because "read-only" with no explanation is the answer that wastes people's
// afternoons.
func (m *Manager) AdoptNow() AdoptReport {
	m.scanOnce()

	rep := AdoptReport{Sessions: m.adoptRows(), Terminals: attach.DetectTerminals()}
	if len(rep.Sessions) == 0 {
		rep.Note = "no agent sessions found on this machine"
	}
	return rep
}

// adoptRows describes every live session and whether muxscale can type into it.
func (m *Manager) adoptRows() []AdoptRow {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var rows []AdoptRow
	for _, s := range m.reg.List() {
		if s.State.Terminal() {
			continue
		}
		_, wrapped := m.wrapped[s.ID]
		tmuxTarget := m.tmuxTargets[s.ID]
		_, termctl := m.termctlTargets[s.ID]
		rows = append(rows, AdoptRow{
			ID: s.ID, AgentID: s.AgentID, Title: s.Title, Cwd: s.Cwd,
			Attachment: s.Attachment, Fidelity: s.Fidelity,
			Writable:       wrapped || tmuxTarget != "" || termctl,
			TranscriptPath: s.TranscriptPath,
		})
	}
	return rows
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
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	m.scanOnceLocked()
}

func (m *Manager) scanOnceLocked() {
	tmuxPanes, panesKnown := m.scanTmux()

	procs, err := attach.Discover()
	if err != nil {
		m.log.Debug("discovery failed", "err", err)
		return
	}
	panes := m.terminalPanes(procs)
	running := map[int]bool{}
	defer func() { m.markEnded(running, tmuxPanes, panesKnown) }()
	for _, p := range procs {
		agentID := m.identify(p.Command, p.Args)
		if agentID == "" || helperProcess(p.Args) {
			continue
		}
		running[p.Pid] = true
		// A wrapped session already has a higher-fidelity attachment under a
		// pid natural key; do not shadow it with a discovery-only entry.
		if m.hasWrappedPid(p.Pid) {
			continue
		}
		natural := "pid:" + strconv.Itoa(p.Pid)
		sess := m.reg.Upsert(agentID, natural, func(s *protocol.Session) {
			if attachRank(s.Attachment) > attachRank(protocol.AttachTranscript) {
				// Something better already owns this session. The scan runs
				// every few seconds, so downgrading here would undo an
				// adoption on the very next tick -- which it did, silently,
				// leaving sessions writable in memory and read-only on the
				// wire.
				s.Cwd = p.Cwd
				return
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
				if own := claudecode.ProcessTranscript(p.Pid, p.Started, p.Cwd); own != "" && own != path {
					// The process says which file is its own. Either the
					// match was a guess that picked a neighbour's, or the
					// session moved to another with /resume or /clear; the
					// history read so far is someone else's, so read again.
					if path != "" {
						m.log.Info("following the transcript a session says is its own",
							"session", sess.ID, "pid", p.Pid, "was", path, "now", own)
						m.Retail(sess.ID, own)
					} else {
						m.reg.Upsert(agentID, natural, func(t *protocol.Session) {
							t.TranscriptPath = own
						})
					}
					path = own
				}
				if path == "" && p.Cwd != "" {
					path = m.matchTranscript(p)
					if path != "" {
						m.reg.Upsert(agentID, natural, func(t *protocol.Session) {
							t.TranscriptPath = path
						})
					}
				}
				m.EnsureTailer(sess.ID, path)
			}
		}

		m.adoptTerminalPane(sess.ID, agentID, natural, p, panes)

		if s, ok := m.reg.Get(sess.ID); ok && s.State == protocol.StateStarting {
			m.reg.SetState(sess.ID, protocol.StateGuess{
				State: protocol.StateUnknown, Confidence: 0.5,
				Reason:   "discovered by process scan; no hook or transcript yet",
				Evidence: "scan:pid=" + strconv.Itoa(p.Pid), At: time.Now(),
			})
		}
	}
}

// helperProcess reports an agent binary running as something other than an
// agent session: Claude Code serving the Chrome extension, or serving MCP.
// Each is a long-lived process with nothing to read or answer, and showed in
// the list as a session that never said anything.
func helperProcess(argv []string) bool {
	for i, a := range argv {
		if a == "--chrome-native-host" {
			return true
		}
		if a == "mcp" && i+1 < len(argv) && argv[i+1] == "serve" {
			return true
		}
	}
	return false
}

// markEnded marks the sessions the scan found earlier, whose process is now
// gone, as ended.
//
// Nothing did. A session whose terminal was closed kept its last state --
// often "needs you" -- until the daemon restarted, which then called it
// detached; and detached it stayed, a hundred-odd rows of agents that had
// exited weeks before. A process that is not running is a fact, not a guess,
// so this says so with full confidence.
//
// Only sessions keyed by their process, or by their tmux pane when the tmux
// scan could say which panes exist: a wrapped one reports its own exit, and
// one known only from its hooks has SessionEnd.
func (m *Manager) markEnded(running map[int]bool, panes map[string]bool, panesKnown bool) {
	now := time.Now()
	for _, s := range m.reg.List() {
		if s.State.Terminal() || s.Attachment == protocol.AttachWrap {
			continue
		}
		var reason, evidence string
		if pid, ok := strings.CutPrefix(s.NaturalKey, "pid:"); ok {
			n, err := strconv.Atoi(pid)
			if err != nil || running[n] {
				continue
			}
			reason, evidence = "the process is no longer running", "scan:gone pid="+pid
		} else if pane, ok := strings.CutPrefix(s.NaturalKey, "tmux:"); ok && panesKnown {
			if panes[pane] {
				continue
			}
			reason, evidence = "its tmux pane no longer has an agent in it", "scan:gone pane="+pane
		} else {
			continue
		}
		m.reg.SetState(s.ID, protocol.StateGuess{
			State: protocol.StateExited, Confidence: 1,
			Reason: reason, Evidence: evidence, At: now,
		})
	}
}

// terminalPanes lists adoptable emulator panes, but only when this scan found
// an agent that could use one.
//
// The enumeration costs an Apple event round trip. On a machine with no
// unadopted agent in a scriptable terminal -- which is every machine where the
// shims did their job -- that round trip buys nothing, and this loop runs every
// few seconds forever.
func (m *Manager) terminalPanes(procs []attach.Process) map[string]attach.TermTarget {
	wanted := false
	for _, p := range procs {
		if strings.HasPrefix(p.WritableVia(), "termctl:") && !m.hasWrappedPid(p.Pid) {
			wanted = true
			break
		}
	}
	if !wanted {
		return nil
	}
	panes, err := attach.AdoptableTerminalPanes()
	if err != nil {
		// Once, not every three seconds. The usual cause is a denied
		// Automation permission, which is a standing condition: repeating it
		// would bury everything else in the log without telling anyone
		// anything new.
		m.termctlWarnOnce.Do(func() {
			m.log.Warn("cannot adopt sessions from the terminal emulator", "err", err)
		})
		return nil
	}
	return panes
}

// adoptTerminalPane gives a discovered session a writable channel through its
// terminal emulator, when it has no better one.
//
// "No better one" is the whole condition: a wrapped PTY and a tmux pane are
// both live streams, and this is a 2 Hz sample of someone else's window. It is
// the mechanism of last resort, and the only one that reaches a session the
// user started before muxscale was running.
func (m *Manager) adoptTerminalPane(sessionID, agentID, natural string, p attach.Process, panes map[string]attach.TermTarget) {
	if len(panes) == 0 {
		return
	}
	dev := attach.DevPath(p.TTY)
	if dev == "" {
		return
	}
	target, ok := panes[dev]
	if !ok {
		return
	}

	m.mu.Lock()
	if _, already := m.termctlTargets[sessionID]; already {
		m.mu.Unlock()
		return
	}
	if m.tmuxTargets[sessionID] != "" || m.wrapped[sessionID] != nil {
		m.mu.Unlock()
		return
	}
	target.Pid, target.Started = p.Pid, p.Started
	m.termctlTargets[sessionID] = target
	m.mu.Unlock()

	m.reg.Upsert(agentID, natural, func(s *protocol.Session) {
		if attachRank(s.Attachment) > attachRank(protocol.AttachTermctl) {
			return
		}
		s.Attachment = protocol.AttachTermctl
		s.Fidelity = protocol.FidelityStructured
	})
	// A restart marks every session it did not launch DETACHED, certainly so,
	// and adopting it again restores the attachment but not the state. Left
	// there, a re-adopted session read as detached until the next hook -- and
	// without hooks, forever. Hand it back to inference, which the transcript
	// tailer answers within a poll.
	if s, ok := m.reg.Get(sessionID); ok && s.State == protocol.StateDetached && s.Guess.Evidence == "restore" {
		m.reg.SetState(sessionID, protocol.StateGuess{
			State: protocol.StateUnknown, Confidence: 0.3,
			Reason:   "adopted again after a restart",
			Evidence: "adopt", At: time.Now(),
		})
	}
	m.log.Info("adopted a session through its terminal",
		"session", sessionID, "terminal", target.Kind, "tty", dev)
	_ = m.store.Audit("muxscale", "session_adopted", sessionID,
		fmt.Sprintf("termctl=%s tty=%s", target.Kind, dev))
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
		m.Drop(other.ID)
	}
}

// Merge folds a session found twice into the one being kept: the duplicate
// goes, and the kept row's transcript is read again from the start of the
// file.
//
// Each row had been recording the conversation only while it was the one
// following the file, so after a merge the kept row's history had a hole
// wherever the other had been recording instead -- an hour of a session
// missing on the phone, with nothing to say so.
func (m *Manager) Merge(keepID, dropID string) {
	path := ""
	if s, ok := m.reg.Get(dropID); ok {
		path = s.TranscriptPath
	}
	if s, ok := m.reg.Get(keepID); ok && s.TranscriptPath != "" {
		path = s.TranscriptPath
	}
	m.Drop(dropID)
	if path != "" {
		m.Retail(keepID, path)
	}
}

// Retail forgets a session's stored transcript and follows its file again
// from the beginning, telling clients to load it afresh.
func (m *Manager) Retail(sessionID, path string) {
	// Under ingestMu, so the old tailer cannot store one more batch after
	// the clear.
	m.ingestMu.Lock()
	m.mu.Lock()
	delete(m.tailers, sessionID) // the running loop sees it is replaced, and stops
	m.mu.Unlock()
	err := m.store.DeleteTranscript(sessionID)
	m.ingestMu.Unlock()
	if err != nil {
		m.log.Warn("could not clear a transcript to read it again", "session", sessionID, "err", err)
		return
	}
	m.reg.Update(sessionID, func(s *protocol.Session) { s.TranscriptPath = path })
	m.reg.ResetTranscript(sessionID)
	m.startTailer(sessionID, path, false)
}

// ingest stores a batch of a session's transcript events, unless tl has been
// replaced as the session's tailer. It reports whether it stored them.
func (m *Manager) ingest(sessionID string, tl *claudecode.Tailer, events []protocol.TranscriptEvent) bool {
	m.ingestMu.Lock()
	defer m.ingestMu.Unlock()
	m.mu.RLock()
	current := m.tailers[sessionID] == tl
	m.mu.RUnlock()
	if !current {
		return false
	}
	if err := m.store.AppendEvents(events); err != nil {
		m.log.Warn("storing transcript events failed", "session", sessionID, "err", err)
	}
	return true
}

// Drop removes a session that turned out to be another one found twice,
// along with everything following it.
func (m *Manager) Drop(id string) {
	m.reg.Remove(id)
	m.mu.Lock()
	delete(m.tailers, id)
	delete(m.termctlTargets, id)
	m.mu.Unlock()
}

// attachRank orders attachments by capability, so a scan that finds a session
// a second way can never trade a live channel for a weaker one.
func attachRank(a protocol.Attachment) int {
	switch a {
	case protocol.AttachNative:
		return 5
	case protocol.AttachWrap:
		return 4
	case protocol.AttachTmux:
		return 3
	case protocol.AttachTermctl:
		return 2
	case protocol.AttachTranscript:
		return 1
	}
	return 0
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

// matchTranscript finds the transcript belonging to a discovered process.
//
// The join is on start time, not on "newest file in the project directory":
// two agents can share one project, and picking the most recently written file
// would hand a freshly discovered session its neighbour's conversation -- which
// looks like success until someone reads the transcript on their phone.
func (m *Manager) matchTranscript(p attach.Process) string {
	if p.Cwd == "" {
		return ""
	}
	cands, err := claudecode.TranscriptCandidates(p.Cwd)
	if err != nil {
		m.log.Debug("listing transcript candidates failed", "cwd", p.Cwd, "err", err)
		return ""
	}
	return claudecode.PickTranscript(cands, p.Started, m.claimedTranscripts())
}

// claimedTranscripts is every transcript file a live session already owns.
//
// Read fresh on each call rather than cached: the discovery loop claims files
// as it goes, and within one scan two processes in the same project would
// otherwise both be matched to the same transcript.
func (m *Manager) claimedTranscripts() map[string]bool {
	claimed := map[string]bool{}
	for _, s := range m.reg.List() {
		if s.TranscriptPath != "" && !s.State.Terminal() {
			claimed[s.TranscriptPath] = true
		}
	}
	return claimed
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
	m.startTailer(sessionID, path, true)
}

// startTailer follows path for a session unless something already does.
// resume picks up after what the store holds; otherwise the file is read from
// its start.
func (m *Manager) startTailer(sessionID, path string, resume bool) {
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
	if !resume {
		go m.tailLoop(sessionID, tl)
		return
	}
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
		// Only its own entry: a replacement started by Retail is not this
		// loop's to remove.
		if m.tailers[sessionID] == tl {
			delete(m.tailers, sessionID)
		}
		m.mu.Unlock()
	}()

	// The newest event seen, for inferring state while the file is quiet.
	// Seeded from the store, since a tailer resumed after a restart starts at
	// the end of the file and would otherwise know nothing until the next line.
	var last []protocol.TranscriptEvent
	if maxSeq, err := m.store.MaxSeq(sessionID); err == nil && maxSeq > 0 {
		last, _ = m.store.Transcript(sessionID, maxSeq-1, 1)
	}

	var misses int
	for range t.C {
		// Ended, dropped as a duplicate, or replaced: nothing left to follow
		// it for.
		if s, ok := m.reg.Get(sessionID); !ok || s.State.Terminal() {
			return
		}
		m.mu.RLock()
		current := m.tailers[sessionID] == tl
		m.mu.RUnlock()
		if !current {
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
		m.applyMeta(sessionID, meta)
		if len(events) == 0 {
			// Nothing new, which is itself news: inference said "running"
			// when the last line landed, and only a later look can see that it
			// went quiet. Without this a session stayed Working forever after
			// its turn ended, unless a hook happened to correct it.
			m.reinfer(sessionID, tl, last)
			continue
		}
		last = events[len(events)-1:]

		for i := range events {
			events[i].SessionID = sessionID
		}
		// Stored here, whole, before anyone is told. They used to reach the
		// store the way they reach phones, through the registry's change
		// feed, which drops what a slow subscriber has no room for: a
		// session's first read, thousands of events at once, kept a few
		// hundred, and the conversation on the phone had holes nothing
		// could fill.
		if !m.ingest(sessionID, tl, events) {
			return // replaced while reading; these belong to a transcript being rebuilt
		}
		for i := range events {
			m.reg.AppendEvent(events[i])
		}

		// Never over a state the agent reported (confidence 1): a guess must
		// not override what the agent told us directly. A hook session's
		// state is a guess only until its first hook after a restart.
		if s, ok := m.reg.Get(sessionID); ok && s.Guess.Confidence < 1 {
			if fi, err := os.Stat(tl.Path()); err == nil {
				guess := claudecode.InferState(events, fi.ModTime(), 6*time.Second)
				m.reg.SetState(sessionID, guess)
			}
		}
	}
}

// reinfer re-runs state inference for a quiet transcript, and records the
// result only when it changes the state. It never replaces a state the agent
// reported (confidence 1) -- a hook's word outranks a guess from silence.
func (m *Manager) reinfer(sessionID string, tl *claudecode.Tailer, last []protocol.TranscriptEvent) {
	if len(last) == 0 {
		return
	}
	s, ok := m.reg.Get(sessionID)
	if !ok || s.Guess.Confidence >= 1 {
		return
	}
	fi, err := os.Stat(tl.Path())
	if err != nil {
		return
	}
	if guess := claudecode.InferState(last, fi.ModTime(), 6*time.Second); guess.State != s.State {
		m.reg.SetState(sessionID, guess)
	}
}

// applyMeta copies what a transcript says about its session onto the row:
// title, name, branch and model, for free, without asking the agent anything.
// Applied whether or not the chunk held any conversation, since a /rename is a
// line of its own.
func (m *Manager) applyMeta(sessionID string, meta claudecode.Meta) {
	if meta.Title == "" && meta.CustomTitle == nil && meta.GitBranch == "" &&
		meta.Model == "" && meta.Version == "" {
		return
	}
	m.reg.Upsert("claude-code", naturalOf(m.reg, sessionID), func(s *protocol.Session) {
		if meta.Title != "" {
			s.Title = meta.Title
		}
		if meta.CustomTitle != nil {
			s.Name = *meta.CustomTitle
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
}

// maxNameLen bounds a session name: long enough for any title a person would
// type, short enough that nobody can park a document in the session list.
const maxNameLen = 120

// Rename gives a session a name of the person's choosing, or clears it when
// name is empty.
//
// For a Claude Code session the name is also written where Claude Code keeps
// its own, so `claude --resume` shows it and a /rename in the terminal and a
// rename from the phone are one setting, not two that drift. The tailer then
// reads that entry back like any other, which keeps the transcript the single
// record. Every other agent's name lives in muxscale only.
func (m *Manager) Rename(sessionID, name, actor string) (*protocol.Session, error) {
	name = strings.Join(strings.Fields(name), " ")
	if utf8.RuneCountInString(name) > maxNameLen {
		return nil, fmt.Errorf("a name can be at most %d characters", maxNameLen)
	}
	sess, ok := m.reg.Get(sessionID)
	if !ok {
		return nil, fmt.Errorf("unknown session %s", sessionID)
	}
	if sess.AgentID == "claude-code" && sess.TranscriptPath != "" {
		agentSession := strings.TrimSuffix(filepath.Base(sess.TranscriptPath), ".jsonl")
		if err := claudecode.SetCustomTitle(sess.TranscriptPath, agentSession, name); err != nil {
			// The name still holds in muxscale; only the terminal side missed it.
			m.log.Warn("could not write the name into the transcript",
				"session", sessionID, "err", err)
		}
	}
	updated := m.reg.Update(sessionID, func(s *protocol.Session) { s.Name = name })
	if updated == nil {
		return nil, fmt.Errorf("unknown session %s", sessionID)
	}
	_ = m.store.Audit(actor, "session_renamed", sessionID, "")
	return updated, nil
}

// naturalOf looks up a session's natural key so Upsert targets the right row.
func naturalOf(reg *Registry, sessionID string) string {
	if s, ok := reg.Get(sessionID); ok {
		return s.NaturalKey
	}
	return sessionID
}

// scanTmux adopts agent panes so already-running sessions accept input. It
// returns the panes with an agent in them, and whether it could tell: no
// tmux, or no tmux server, is an answer (none), but a failed listing is not.
func (m *Manager) scanTmux() (map[string]bool, bool) {
	found := map[string]bool{}
	if !attach.TmuxAvailable() {
		return found, true
	}
	panes, err := attach.FindAgentPanes()
	if err != nil {
		m.log.Debug("tmux scan failed", "err", err)
		return found, false
	}
	for _, p := range panes {
		agentID := m.identify(p.Command, nil)
		if agentID == "" {
			continue
		}
		found[p.PaneID] = true
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

		// Starting, or ended before and running again: a pane outlives the
		// agent in it, and a new one started there reuses its key.
		if s, ok := m.reg.Get(sess.ID); ok && (s.State == protocol.StateStarting || s.State.Terminal()) {
			m.reg.SetState(sess.ID, protocol.StateGuess{
				State: protocol.StateUnknown, Confidence: 0.5,
				Reason:   "adopted from tmux; no hook or transcript yet",
				Evidence: "tmux:" + p.PaneID, At: time.Now(),
			})
		}
		if agentID == "claude-code" && p.Path != "" {
			if path := m.matchTranscript(attach.Process{Cwd: p.Path}); path != "" {
				m.reg.Upsert(agentID, natural, func(t *protocol.Session) {
					t.TranscriptPath = path
				})
				m.EnsureTailer(sess.ID, path)
			}
		}
	}
	return found, true
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
