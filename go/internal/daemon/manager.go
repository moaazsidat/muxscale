package daemon

import (
	"context"
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
	// persistStop ends persistLoop's subscription; persistDone closes once
	// the loop has written what it had. See StopPersisting.
	persistStop func()
	persistDone chan struct{}
	persistOnce sync.Once

	reg      *Registry
	adapters *adapter.Registry
	store    *store.Store
	log      *slog.Logger

	mu sync.RWMutex
	// ingestMu orders storing a tailer's events against clearing them for a
	// rebuild.
	ingestMu sync.Mutex
	wrapped  map[string]*attach.Wrapped // muxscale session id -> pty
	tailers  map[string]*tailHandle     // muxscale session id -> transcript tailer
	screens  map[string]*vt.Emulator    // muxscale session id -> screen state

	// sampled is guarded by screenMu, not mu. See sampleScreen.
	//
	// sampled holds a screen reconstructed from an adopted terminal's own
	// idea of its contents, for sessions muxscale has no pty for. Kept
	// between polls rather than rebuilt each time so the grid-diffing that
	// drives the stream still produces small deltas.
	sampled map[string]*vt.Emulator

	// screenChanged holds, for each session whose screen someone is
	// watching, a channel closed the next time that screen changes. Guarded
	// by screenMu. This is what lets the stream send a keystroke's echo the
	// moment it arrives, rather than on the next tick of a poll.
	screenChanged map[string]chan struct{}

	// typedAudit is when each session last had typing recorded in the audit
	// log. Guarded by mu. See SendRaw.
	typedAudit map[string]time.Time

	// answerMu serialises answers typed for hook-reported approvals, so two
	// taps at once cannot type two answers (see AnswerByKeys).
	answerMu sync.Mutex
	// typeKeys writes an answer's keys; deliver, except in tests, which have
	// no terminal to type into.
	typeKeys func(sessionID string, keys []byte) error

	// closing records sessions a human asked to end, so their exit-by-signal is
	// reported as "closed" rather than "crashed".
	closing map[string]bool

	// discover lists the agent processes running now, and parentOf names a
	// process's parent: the process table, swapped out in tests.
	discover func() ([]attach.Process, error)
	parentOf func(pid int) int

	// tmuxTargets maps a session to an adopted tmux pane, which is how an
	// already-running session becomes writable without changing anyone's habits.
	tmuxTargets map[string]string

	// termctlTargets maps a session to a terminal-emulator pane adopted through
	// that emulator's scripting interface. This is the last resort and the one
	// that covers the common case: an agent already running in an ordinary
	// terminal window, started long before the daemon, in no multiplexer.
	termctlTargets map[string]attach.TermTarget
	// questionMu makes posting a keyboard question one step with checking
	// that none is up, so two polls cannot post it twice.
	questionMu sync.Mutex
	// typedQuestions are the tool calls of questions answered by typing.
	typedQuestions map[string]bool

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

	// lastAdapterScan throttles adapter enumeration. The process scan is one
	// `ps`; an adapter's Discover reads session directories and sometimes
	// every file header, and running that for every agent every three seconds
	// would make an idle daemon the busiest thing on the machine.
	lastAdapterScan time.Time
}

// adapterScanEvery is how often adapters enumerate their own sessions. Slow
// enough to be free, fast enough that a session started while the phone is
// open appears without a refresh.
const adapterScanEvery = 30 * time.Second

// NewManager wires a registry to durable storage.
//
// adapters may be nil, in which case agent identification falls back to the
// binary-name table alone. Tests that do not care about adapter dispatch pass
// nil rather than constructing the whole built-in set.
func NewManager(reg *Registry, adapters *adapter.Registry, st *store.Store, log *slog.Logger) *Manager {
	m := &Manager{
		reg: reg, adapters: adapters, store: st, log: log,
		wrapped:        map[string]*attach.Wrapped{},
		tailers:        map[string]*tailHandle{},
		screens:        map[string]*vt.Emulator{},
		closing:        map[string]bool{},
		tmuxTargets:    map[string]string{},
		termctlTargets: map[string]attach.TermTarget{},
		typedQuestions: map[string]bool{},
		sampled:        map[string]*vt.Emulator{},
		screenChanged:  map[string]chan struct{}{},
		typedAudit:     map[string]time.Time{},
	}
	m.typeKeys = m.deliver
	m.discover = attach.Discover
	m.parentOf = parentPid
	ch, cancel := m.reg.SubscribeInternal()
	m.persistStop, m.persistDone = cancel, make(chan struct{})
	go m.persistLoop(ch)
	go m.screenQueueLoop()
	return m
}

// tailHandle is one session's transcript stream, wrapped so the manager can
// tell a replacement from the stream it started without relying on interface
// identity (which panics for a non-comparable dynamic type).
type tailHandle struct {
	stream adapter.TranscriptStream
	ref    string
}

// defaultPoll is the transcript cadence for adapters that do not declare one.
const defaultPoll = 750 * time.Millisecond

// persistLoop mirrors registry changes into SQLite. It runs in its own
// goroutine so a slow disk cannot stall a hook handler that is holding an
// agent's request open.
func (m *Manager) persistLoop(ch <-chan Change) {
	defer close(m.persistDone)
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
		// Only a wrapped session is certainly lost: the daemon owned its PTY
		// and that PTY died with it. Every other attachment can be found
		// again -- by the process scan, a hook, a terminal, or the agent's
		// own session files -- so call it unknown and let re-adoption say
		// what it is. Marking those DETACHED with confidence 1 was a certain
		// lie, and it was final: inference refuses to overwrite confidence 1,
		// so a session whose process was still running stayed buried in the
		// app's collapsed Detached section until it happened to fire a hook.
		state := protocol.StateDetached
		reason := "daemon restarted; the PTY it owned is gone"
		confidence := 1.0
		if s.Attachment != protocol.AttachWrap {
			state = protocol.StateUnknown
			reason = "daemon restarted; awaiting re-adoption"
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
	// Env is the process's environment; nil inherits the daemon's.
	Env []string
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
		Env: opt.Env,
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
		// A login shell, as a terminal app opens: it reads .zprofile, which
		// is where Homebrew and most version managers put themselves on PATH.
		// Without it a daemon started at login, with launchd's bare PATH,
		// opened a shell where half your commands were missing.
		Command: []string{loginShell(), "-l"},
		Env:     terminalEnv(os.Environ()),
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

// terminalEnv is the environment a terminal app gives the shell it opens,
// filling in what a daemon may have been started without.
//
// TERM above all: with none, or "dumb", zsh's line editor and its plugins --
// autosuggestions, syntax highlighting, completion menus -- turn themselves
// off, and the phone's terminal is a line printer. The emulator speaks
// xterm-256color, so that is what the shell is told. What the user set
// themselves is kept.
func terminalEnv(env []string) []string {
	have := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			have[k] = v
		}
	}
	out := make([]string, 0, len(env)+4)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "TERM":
			continue // set below, always
		case "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "TERM_SESSION_ID", "ITERM_SESSION_ID", "KITTY_WINDOW_ID", "WEZTERM_PANE", "TMUX", "TMUX_PANE":
			// The daemon's own terminal, if it was started in one. A shell
			// told it is in iTerm2 sends iTerm2's private escapes.
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "TERM=xterm-256color")
	if have["COLORTERM"] == "" {
		out = append(out, "COLORTERM=truecolor")
	}
	if have["LANG"] == "" && have["LC_ALL"] == "" && have["LC_CTYPE"] == "" {
		// Without a UTF-8 locale zsh draws every non-ASCII character as an
		// escape, and prompts are full of them.
		out = append(out, "LANG=en_US.UTF-8")
	}
	return out
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
		m.screenChangedLocked(sessionID)
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

// ScreenChanged returns a channel closed the next time a session's screen
// changes or its process ends. Only sessions muxscale holds the PTY for
// report changes; the others never close it, and are read by polling.
func (m *Manager) ScreenChanged(sessionID string) <-chan struct{} {
	m.screenMu.Lock()
	defer m.screenMu.Unlock()
	ch := m.screenChanged[sessionID]
	if ch == nil {
		ch = make(chan struct{})
		m.screenChanged[sessionID] = ch
	}
	return ch
}

// HasLiveScreen reports whether muxscale holds the session's PTY, and so
// whether ScreenChanged will ever fire for it.
func (m *Manager) HasLiveScreen(sessionID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.screens[sessionID] != nil
}

// screenChangedLocked wakes whoever is waiting on a session's screen. The
// caller holds screenMu.
func (m *Manager) screenChangedLocked(sessionID string) {
	if ch := m.screenChanged[sessionID]; ch != nil {
		close(ch)
		delete(m.screenChanged, sessionID)
	}
}

func (m *Manager) notifyScreen(sessionID string) {
	m.screenMu.Lock()
	m.screenChangedLocked(sessionID)
	m.screenMu.Unlock()
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
			replies := em.TakeReplies()
			m.screenChangedLocked(sessionID)
			m.screenMu.Unlock()
			if len(replies) > 0 {
				_, _ = w.Write(replies)
			}
			m.reg.NoteOutput(sessionID, n)
		}
		if err != nil {
			break
		}
	}
	<-w.Exited()
	defer m.notifyScreen(sessionID) // a watcher learns the screen is gone

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
	pane := m.tmuxTargets[sessionID]
	m.mu.RUnlock()
	if !ok && pane == "" {
		return nil, false
	}
	var text string
	var err error
	if ok {
		text, err = term.Text()
	} else {
		// A tmux pane muxscale adopted: what it shows now, as plain text.
		var out []byte
		out, err = attach.CapturePaneText(pane)
		text = strings.TrimRight(string(out), "\n")
	}
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
		b, err := attach.EncodeKeyMode(key, m.appCursor(sessionID))
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

// Interrupt stops what the session is doing, the way a person at its terminal
// would: Esc for an agent's TUI, which every one muxscale reads treats as "stop
// this turn" (Claude Code, Codex, omp, OpenCode, Gemini CLI, Copilot CLI), and
// Ctrl-C for a shell. The client says "interrupt" and the daemon picks the key,
// so an agent that needs something else is a change here, not in every client.
//
// Not fenced: an interrupt is aimed at whatever is running now, and a turn that
// moved on since the button was drawn is exactly the thing to stop. Refused
// while an approval is pending, where Esc would answer the prompt behind
// muxscale's back while the hook still holds it open; deny it instead.
func (m *Manager) Interrupt(sessionID, actor string) error {
	sess, ok := m.reg.Get(sessionID)
	if !ok {
		return fmt.Errorf("unknown session %s", sessionID)
	}
	switch sess.State {
	case protocol.StateExited, protocol.StateCrashed:
		return fmt.Errorf("the session has ended")
	case protocol.StateWaitingApproval:
		return fmt.Errorf("the agent is waiting on an approval; deny it to stop")
	}
	key := interruptKey(sess.AgentID)
	b, err := attach.EncodeKeyMode(key, m.appCursor(sessionID))
	if err != nil {
		return err
	}
	if err := m.deliver(sessionID, b); err != nil {
		return err
	}
	_ = m.store.Audit(actor, "interrupt", sessionID, "key="+string(key))
	return nil
}

// interruptKey is the key that stops a session's current work.
func interruptKey(agentID string) attach.NamedKey {
	if agentID == ShellAgentID {
		return attach.KeyCtrlC
	}
	return attach.KeyEscape
}

// SendRaw writes bytes to a session's terminal exactly as given: a keystroke
// from a terminal on the phone, which is not a reply to an agent and so is
// neither fenced nor normalised. Arrow keys arrive as named keys through
// SendInput instead, since only the daemon knows which encoding the program
// asked for.
//
// Every keystroke is not an audit entry -- a typed command would be thirty
// rows -- so typing is recorded once a minute per session at most.
func (m *Manager) SendRaw(sessionID string, data []byte, actor string) error {
	if _, ok := m.reg.Get(sessionID); !ok {
		return fmt.Errorf("unknown session %s", sessionID)
	}
	if len(data) == 0 {
		return nil
	}
	if err := m.deliver(sessionID, data); err != nil {
		return err
	}
	if !m.HasLiveScreen(sessionID) {
		go m.lookAfterTyping(sessionID)
	}
	now := time.Now()
	m.mu.Lock()
	due := now.Sub(m.typedAudit[sessionID]) >= time.Minute
	if due {
		m.typedAudit[sessionID] = now
	}
	m.mu.Unlock()
	if due {
		_ = m.store.Audit(actor, "input_typed", sessionID, "keystrokes from a terminal")
	}
	return nil
}

// typedLooks are when a screen muxscale polls is read again after a
// keystroke: soon, for a program that echoes as it reads, and once more for
// one that redraws on a timer of its own (Claude Code's input box). Each read
// of an iTerm2 pane is an osascript, about a tenth of a second, so the second
// is the last.
var typedLooks = []time.Duration{30 * time.Millisecond, 200 * time.Millisecond}

// lookAfterTyping wakes the streams polling a session's screen, so what was
// typed shows on the phone when it is on the terminal, not on the next tick.
func (m *Manager) lookAfterTyping(sessionID string) {
	var waited time.Duration
	for _, at := range typedLooks {
		time.Sleep(at - waited)
		waited = at
		m.notifyScreen(sessionID)
	}
}

// PasteBytes encodes pasted text for a session's terminal: inside paste
// brackets if the program enabled them, which is how a shell knows not to run
// each line of a pasted block as it arrives. A program that has not asked for
// them would show the brackets as text, so it gets the lines as typed.
func (m *Manager) PasteBytes(sessionID, text string) []byte {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	m.mu.RLock()
	em := m.screens[sessionID]
	m.mu.RUnlock()
	bracketed := false
	if em != nil {
		m.screenMu.Lock()
		bracketed = em.BracketedPaste
		m.screenMu.Unlock()
	}
	if bracketed {
		return []byte("\x1b[200~" + text + "\x1b[201~")
	}
	return []byte(strings.ReplaceAll(text, "\n", "\r"))
}

// appCursor reports whether the program in a session asked for application
// cursor keys, which changes the bytes an arrow key is.
func (m *Manager) appCursor(sessionID string) bool {
	m.mu.RLock()
	em := m.screens[sessionID]
	m.mu.RUnlock()
	if em == nil {
		return false
	}
	m.screenMu.Lock()
	defer m.screenMu.Unlock()
	return em.AppCursor
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

// Kill ends a session: one muxscale runs, or an agent started inside a
// shell muxscale runs (see stopAgentInShell).
func (m *Manager) Kill(sessionID string) error {
	m.mu.RLock()
	w := m.wrapped[sessionID]
	m.mu.RUnlock()
	if w == nil {
		return m.stopAgentInShell(sessionID)
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

	procs, err := m.discover()
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
		// An adapter that can join a process to a transcript gets its
		// transcript followed, which is what turns a bare process listing
		// into a readable session. The join itself is the adapter's: Claude
		// Code has a pid record, omp a terminal breadcrumb, and neither
		// belongs in the daemon.
		if a, ok := m.adapterByID(agentID); ok {
			if loc, ok := a.(adapter.Locator); ok {
				if s, ok := m.reg.Get(sess.ID); ok {
					path := s.TranscriptPath
					if own := loc.TranscriptForProcess(p.ForAdapter()); own != "" && own != path {
						// The process says which file is its own. Either the
						// match was a guess that picked a neighbour's, or the
						// session moved to another with /resume or /clear; the
						// history read so far is someone else's, so read again.
						if path != "" {
							m.log.Info("following the transcript a session says is its own",
								"session", sess.ID, "pid", p.Pid, "was", path, "now", own)
							m.Retail(sess.ID, own)
						} else {
							m.reg.Update(sess.ID, func(t *protocol.Session) {
								t.TranscriptPath = own
							})
						}
						path = own
					}
					if path == "" && p.Cwd != "" {
						path = loc.MatchTranscript(p.Cwd, p.Started, m.claimedTranscripts())
						if path != "" {
							m.reg.Update(sess.ID, func(t *protocol.Session) {
								t.TranscriptPath = path
							})
						}
					}
					m.EnsureTailer(sess.ID, path)
				}
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
	m.discoverAdapters()
}

// discoverAdapters asks every adapter to enumerate its own sessions.
//
// This is the path the process scan by construction cannot take: a session
// whose process is not in this process table -- another host's, a remote
// workspace, one that ended while the daemon was down -- is still a file the
// agent wrote. Discovery is throttled because it is the most expensive thing
// in the scan, and a session file does not appear every three seconds.
func (m *Manager) discoverAdapters() {
	if m.adapters == nil || time.Since(m.lastAdapterScan) < adapterScanEvery {
		return
	}
	m.lastAdapterScan = time.Now()

	for _, a := range m.adapters.All() {
		d, ok := a.(adapter.Discoverer)
		if !ok {
			continue
		}
		found, err := d.Discover(context.Background())
		if err != nil {
			m.log.Debug("adapter discovery failed", "agent", a.Info().ID, "err", err)
			continue
		}
		live := map[string]bool{}
		for _, s := range found {
			// History is not a live session. The process scan and the store
			// already keep what ended; inserting every file an agent ever
			// wrote would put months of old conversations in `ls` on first
			// run. A discovered session is adopted only while its agent still
			// considers it alive.
			if !s.Live {
				continue
			}
			live[s.NaturalKey] = true
			m.adoptDiscovered(a, s)
		}
		m.retireDiscovered(a.Info().ID, live)
	}
}

// retireDiscovered ends the rows discovery made that their agent no longer
// considers alive.
//
// Discovery is the only evidence such a row has: no process, no pane, no hook.
// It used to only ever add, so a row outlived the window that let it in -- a
// Codex thread stayed "waiting for you" a day after its window closed, and a
// `codex exec` run that answered once and exited stayed "working" -- because
// nothing else was ever going to say it had ended.
func (m *Manager) retireDiscovered(agentID string, live map[string]bool) {
	now := time.Now()
	for _, s := range m.reg.List() {
		if s.AgentID != agentID || s.State.Terminal() || s.Attachment != protocol.AttachTranscript {
			continue
		}
		if strings.HasPrefix(s.NaturalKey, "pid:") || strings.HasPrefix(s.NaturalKey, "tmux:") {
			continue // the process scan answers for these
		}
		if live[s.NaturalKey] {
			continue
		}
		m.reg.SetState(s.ID, protocol.StateGuess{
			State: protocol.StateExited, Confidence: 0.8,
			Reason:   "no longer active in the agent's session files",
			Evidence: "adapter:" + agentID + " not live", At: now,
		})
	}
}

// adoptDiscovered adds or refreshes one session an adapter enumerated.
//
// The same conversation can already be present under a process key. The
// transcript path is the identity that survives that: if another row is
// already following this file, the new natural key is an alias for it rather
// than a second row for one session.
func (m *Manager) adoptDiscovered(a adapter.Adapter, d adapter.Discovered) {
	info := a.Info()
	if d.NaturalKey == "" {
		return
	}
	if d.Ref != "" {
		if others := m.reg.FindByTranscript(d.Ref); len(others) > 0 {
			keep := others[0]
			m.reg.Alias(info.ID, d.NaturalKey, keep.ID)
			m.reg.Update(keep.ID, func(s *protocol.Session) {
				if s.Cwd == "" {
					s.Cwd = d.Cwd
				}
				// The index is the agent's own title record. A placeholder
				// written by an older discovery pass must yield to it.
				if d.Title != "" {
					s.Title = d.Title
				}
				if s.Name == "" {
					s.Name = d.Name
				}
				if s.Model == "" {
					s.Model = d.Model
				}
			})
			if d.Live {
				m.reviveRestored(keep.ID)
			}
			m.EnsureTailer(keep.ID, d.Ref)
			return
		}
	}

	state := protocol.StateUnknown
	reason := "found in the agent's session files; awaiting a transcript"
	confidence := 0.4
	if !d.Live {
		// Historical sessions are kept (the store retains them) but are not
		// live rows. Claiming otherwise would put every conversation this
		// machine ever had at the top of someone's phone.
		state = protocol.StateExited
		reason = "the session's process is no longer running"
		confidence = 0.8
	}
	sess := m.reg.Upsert(info.ID, d.NaturalKey, func(s *protocol.Session) {
		if attachRank(s.Attachment) > attachRank(protocol.AttachTranscript) {
			return
		}
		if d.Cwd != "" {
			s.Cwd = d.Cwd
		}
		if d.Title != "" {
			s.Title = d.Title
		}
		if d.Name != "" {
			s.Name = d.Name
		}
		if d.Model != "" {
			s.Model = d.Model
		}
		if d.Ref != "" {
			s.TranscriptPath = d.Ref
		}
		s.Attachment = protocol.AttachTranscript
		s.Fidelity = protocol.FidelityStructured
	})
	if existing, ok := m.reg.Get(sess.ID); ok {
		revive := existing.State == protocol.StateStarting ||
			(d.Live && existing.State == protocol.StateDetached && existing.Guess.Evidence == "restore") ||
			(d.Live && existing.State == protocol.StateUnknown && existing.Guess.Confidence < 1) ||
			(d.Live && retired(existing))
		if revive {
			m.reg.SetState(sess.ID, protocol.StateGuess{
				State: state, Confidence: confidence, Reason: reason,
				Evidence: "adapter:" + info.ID, At: time.Now(),
			})
		}
	}
	if d.Ref != "" {
		m.EnsureTailer(sess.ID, d.Ref)
	}
}

// retired reports a row that retireDiscovered ended. Resuming the
// conversation -- `codex resume` -- makes it live again.
func retired(s protocol.Session) bool {
	return s.State == protocol.StateExited && strings.HasSuffix(s.Guess.Evidence, " not live")
}

// reviveRestored gives a live session back to inference after a restart.
//
// Restore has to put something in the state column, and an older build put
// DETACHED there with confidence 1 -- a claim inference is not allowed to
// overwrite. Discovery seeing the agent still holds the session is the fact
// that lifts it: unknown, at the confidence of a guess, so the transcript
// can say what the session is actually doing.
func (m *Manager) reviveRestored(sessionID string) {
	s, ok := m.reg.Get(sessionID)
	if !ok {
		return
	}
	switch {
	case s.State == protocol.StateDetached && s.Guess.Evidence == "restore",
		s.State == protocol.StateStarting,
		s.State == protocol.StateUnknown && s.Guess.Confidence < 1,
		retired(s):
		m.reg.SetState(sessionID, protocol.StateGuess{
			State: protocol.StateUnknown, Confidence: 0.4,
			Reason:   "found live in the agent's session files",
			Evidence: "adapter-discovery", At: time.Now(),
		})
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
		// omp runs long-lived worker processes under its own binary
		// (__omp_worker_daemon_broker, __omp_worker_text_predict). They hold no
		// session and have nothing to read or answer, but the process scan sees
		// the same argv[0] as an agent and would list them forever.
		if strings.HasPrefix(a, "__omp_worker_") {
			return true
		}
	}
	// Codex's app-server and remote-control daemons are infrastructure, not
	// conversations. They outlive every session and would sit in the list.
	if len(argv) > 1 && filepath.Base(argv[0]) == "codex" {
		switch argv[1] {
		case "app-server", "remote-control", "exec-server":
			return true
		}
	}
	return false
}

// adapterByID returns the registered adapter for an id, or nil in tests that
// run without a registry.
func (m *Manager) adapterByID(id string) (adapter.Adapter, bool) {
	if m.adapters == nil {
		return nil, false
	}
	return m.adapters.Get(id)
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
		if capabilityRank(other) > capabilityRank(keep) {
			// A stronger row owns this conversation. The caller is a process
			// scan or hook that should have found the stronger attachment;
			// dropping the weaker row here would trade away write access.
			continue
		}
		m.log.Debug("folding a duplicate session",
			"kept", keepID, "dropped", other.ID, "transcript", transcriptPath)
		m.Merge(keepID, other.ID)
	}
}

// capabilityRank orders two rows for the same conversation by what they can
// do, attachment first. Fidelity alone is not enough: a transcript-only row
// and a terminal-adopted row are both STRUCTURED, and the adopted one is the
// row a reply can actually reach.
func capabilityRank(s protocol.Session) int {
	return attachRank(s.Attachment)*10 + fidelityRank(s.Fidelity)
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
	drop, dropOK := m.reg.Get(dropID)
	path := ""
	if dropOK {
		path = drop.TranscriptPath
	}
	keep, keepOK := m.reg.Get(keepID)
	if keepOK && keep.TranscriptPath != "" {
		path = keep.TranscriptPath
	}
	// The duplicate may be the row carrying what a person set or read:
	// a rename made while that row was the one on screen, a title recovered
	// from the file. Dropping it without folding these in is how a session
	// loses its name while still being the same conversation.
	if keepOK && dropOK {
		m.reg.Update(keepID, func(s *protocol.Session) {
			if s.Name == "" {
				s.Name = drop.Name
			}
			if s.Title == "" {
				s.Title = drop.Title
			}
			if s.Cwd == "" {
				s.Cwd = drop.Cwd
			}
			if s.Model == "" {
				s.Model = drop.Model
			}
			if s.GitBranch == "" {
				s.GitBranch = drop.GitBranch
			}
			if s.AgentVer == "" {
				s.AgentVer = drop.AgentVer
			}
			if s.PromptID == "" {
				s.PromptID = drop.PromptID
			}
		})
	}
	// The dropped row's keys lead to the kept one, and its terminal pane
	// goes with them. Without this, the next scan found no row for the
	// process, made a new one and adopted the pane again, and the next hook
	// folded that away: a session in kitty changed id every few seconds while
	// it worked, and replies went to ids already gone.
	if keepOK {
		m.reg.Redirect(dropID, keepID)
		m.mu.Lock()
		if t, ok := m.termctlTargets[dropID]; ok {
			if _, has := m.termctlTargets[keepID]; !has && m.tmuxTargets[keepID] == "" && m.wrapped[keepID] == nil {
				m.termctlTargets[keepID] = t
			}
		}
		m.mu.Unlock()
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

// ingest stores a batch of a session's transcript events, unless h has been
// replaced as the session's tailer. It reports whether it stored them.
func (m *Manager) ingest(sessionID string, h *tailHandle, events []protocol.TranscriptEvent) bool {
	m.ingestMu.Lock()
	defer m.ingestMu.Unlock()
	m.mu.RLock()
	current := m.tailers[sessionID] == h
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

// matchTranscript finds the transcript belonging to a process whose start time
// is unknown -- a tmux pane, whose scan reports only a path. The adapter's
// join still refuses to guess between two equally plausible files.
func (m *Manager) matchTranscript(agentID, cwd string) string {
	if cwd == "" {
		return ""
	}
	a, ok := m.adapterByID(agentID)
	if !ok {
		return ""
	}
	loc, ok := a.(adapter.Locator)
	if !ok {
		return ""
	}
	return loc.MatchTranscript(cwd, time.Time{}, m.claimedTranscripts())
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
	s, ok := m.reg.Get(sessionID)
	if !ok {
		return
	}
	a, ok := m.adapterByID(s.AgentID)
	if !ok {
		return
	}
	tr, ok := a.(adapter.Transcriber)
	if !ok {
		return
	}
	stream, err := tr.OpenTranscript(context.Background(), path)
	if err != nil {
		m.log.Debug("opening transcript failed", "session", sessionID, "agent", s.AgentID, "err", err)
		return
	}
	h := &tailHandle{stream: stream, ref: path}

	m.mu.Lock()
	if _, running := m.tailers[sessionID]; running {
		m.mu.Unlock()
		_ = stream.Close()
		return
	}
	m.tailers[sessionID] = h
	m.mu.Unlock()

	// Resume from what the store already has rather than re-reading a file
	// that can be hundreds of megabytes.
	if resume {
		if maxSeq, err := m.store.MaxSeq(sessionID); err == nil && maxSeq > 0 {
			if seeker, ok := stream.(adapter.SeekableStream); ok {
				if err := seeker.SeekEnd(maxSeq); err != nil {
					m.log.Debug("tailer seek failed", "session", sessionID, "err", err)
				}
			}
		}
	}
	go m.tailLoop(sessionID, h)
}

func (m *Manager) tailLoop(sessionID string, h *tailHandle) {
	interval := defaultPoll
	if paced, ok := h.stream.(adapter.PacedStream); ok {
		interval = paced.Interval()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	defer func() {
		_ = h.stream.Close()
		m.mu.Lock()
		// Only its own entry: a replacement started by Retail is not this
		// loop's to remove.
		if m.tailers[sessionID] == h {
			delete(m.tailers, sessionID)
		}
		m.mu.Unlock()
	}()

	// The newest few events seen, for inferring state while the file is
	// quiet. Several rather than one: an agent can follow the end of a turn
	// with bookkeeping (omp writes a mode change after its final reply), and
	// an adapter that saw only that would have nothing to say about the turn.
	// Seeded from the store, since a tailer resumed after a restart starts at
	// the end of the file and would otherwise know nothing until the next line.
	var last []protocol.TranscriptEvent
	if maxSeq, err := m.store.MaxSeq(sessionID); err == nil && maxSeq > 0 {
		last, _ = m.store.Transcript(sessionID, maxSeq-min(maxSeq, inferWindow), inferWindow)
	}

	var misses int
	for range t.C {
		// Dropped as a duplicate, or replaced: nothing left to follow it for.
		s, ok := m.reg.Get(sessionID)
		if !ok {
			return
		}
		m.mu.RLock()
		current := m.tailers[sessionID] == h
		m.mu.RUnlock()
		if !current {
			return
		}
		// A terminal session gets one more poll before the loop stops. The
		// agent may have ended before the file was flushed (a short print-mode
		// run does exactly this), and stopping on the state alone left a
		// finished session with a hook-recorded state and no transcript.
		terminal := s.State.Terminal()
		events, err := h.stream.Poll(context.Background())
		if err != nil {
			if terminal {
				return
			}
			misses++
			if misses > 20 { // file gone for ~15s; stop following it
				return
			}
			continue
		}
		misses = 0
		if ms, ok := h.stream.(adapter.MetaSource); ok {
			m.applyMeta(sessionID, ms.Meta())
		}
		if len(events) == 0 {
			if terminal {
				return
			}
			// Nothing new, which is itself news: inference said "running"
			// when the last line landed, and only a later look can see that it
			// went quiet. Without this a session stayed Working forever after
			// its turn ended, unless a hook happened to correct it.
			m.reinfer(sessionID, h, last)
			m.syncKeyboardQuestion(sessionID, last, true)
			continue
		}
		last = append(last, events...)
		if len(last) > inferWindow {
			last = append([]protocol.TranscriptEvent(nil), last[len(last)-inferWindow:]...)
		}

		for i := range events {
			events[i].SessionID = sessionID
		}
		// Stored here, whole, before anyone is told. They used to reach the
		// store the way they reach phones, through the registry's change
		// feed, which drops what a slow subscriber has no room for: a
		// session's first read, thousands of events at once, kept a few
		// hundred, and the conversation on the phone had holes nothing
		// could fill.
		if !m.ingest(sessionID, h, events) {
			return // replaced while reading; these belong to a transcript being rebuilt
		}
		m.syncKeyboardQuestion(sessionID, last, false)
		for i := range events {
			m.reg.AppendEvent(events[i])
		}

		// Never over a state the agent reported (confidence 1): a guess must
		// not override what the agent told us directly. A hook session's
		// state is a guess only until its first hook after a restart.
		if !terminal {
			if guess, ok := m.infer(sessionID, h, last); ok {
				m.reg.SetState(sessionID, guess)
			}
		}
		if terminal {
			return
		}
	}
}

// inferWindow is how many of a transcript's newest events state inference
// is shown.
const inferWindow = 8

// reinfer re-runs state inference for a quiet transcript, and records the
// result only when it changes the state. A state the agent reported
// (confidence 1) is replaced only as infer allows: a hook's word outranks a
// guess from silence.
func (m *Manager) reinfer(sessionID string, h *tailHandle, last []protocol.TranscriptEvent) {
	s, ok := m.reg.Get(sessionID)
	if !ok {
		return
	}
	if guess, ok := m.infer(sessionID, h, last); ok && guess.State != s.State {
		m.reg.SetState(sessionID, guess)
	}
}

// infer asks the session's adapter what the transcript means.
//
// The daemon knows the evidence -- the recent events, how long the file has
// been quiet, whether the agent's runtime still holds the session -- and the
// adapter knows how to read it. An adapter with no opinion returns false, and
// the session keeps whatever state it had rather than being guessed at by a
// generic heuristic that knows nothing about the agent.
func (m *Manager) infer(sessionID string, h *tailHandle, events []protocol.TranscriptEvent) (protocol.StateGuess, bool) {
	// A rollout can hold lifecycle records and no rendered conversation yet
	// -- a failed first turn is exactly that. The adapter's own last event is
	// still evidence, so an empty tail is not an empty question.
	if len(events) == 0 {
		er, ok := h.stream.(adapter.EventReporter)
		if !ok || er.LastEvent() == "" {
			return protocol.StateGuess{}, false
		}
	}
	s, ok := m.reg.Get(sessionID)
	if !ok {
		return protocol.StateGuess{}, false
	}
	a, ok := m.adapterByID(s.AgentID)
	if !ok {
		return protocol.StateGuess{}, false
	}
	si, ok := a.(adapter.StateInferrer)
	if !ok {
		return protocol.StateGuess{}, false
	}
	path := h.ref
	if path == "" {
		path = s.TranscriptPath
	}
	fi, err := os.Stat(path)
	if err != nil {
		return protocol.StateGuess{}, false
	}
	in := adapter.StateInput{
		Ref: path, Recent: events,
		Idle: time.Since(fi.ModTime()), Quiescence: 6 * time.Second,
	}
	if er, ok := h.stream.(adapter.EventReporter); ok {
		in.Event = er.LastEvent()
	}
	if lv, ok := a.(adapter.Liveness); ok {
		if live, known := lv.Live(path); known {
			in.Live = &live
		}
	}
	exact := s.Guess.Confidence >= 1
	if exact && !staleRunning(s, fi.ModTime(), in.Idle) {
		return protocol.StateGuess{}, false
	}
	g, ok := si.InferState(in)
	if exact && ok && !g.State.Terminal() && g.State != protocol.StateWaitingInput {
		// Only the end of a turn may replace what a hook reported; a guess
		// that the agent is still busy adds nothing to its own word.
		return protocol.StateGuess{}, false
	}
	if ok && g.State.Waiting() && len(events) > 0 {
		// A transcript read can come long after the agent stopped -- after a
		// restart, or after something touched the file and it looked busy for
		// a moment. The agent started waiting when it last wrote, and that is
		// what the phone orders its Needs you list by.
		g.Since = events[len(events)-1].At
	}
	return g, ok
}

// staleHookAfter is how long a transcript must have been quiet, after being
// written since a hook said the agent was working, before the transcript's
// reading of the end of a turn is believed over the hook. Long enough that an
// agent pausing mid-turn to think is not taken for one that stopped.
const staleHookAfter = 60 * time.Second

// staleRunning reports whether a hook's RUNNING has outlived its evidence.
//
// A hook says a turn started; a later hook says it ended. When that later one
// never arrives -- Claude Code sends no Stop when a turn is interrupted with
// Esc, and an extension's post can fail -- the session showed Working until
// the next prompt, however long that was. The transcript is the agent's own
// record too, so once it has been written after the hook and has since gone
// quiet, its reading of the turn may stand. Only RUNNING is ever reopened: an
// approval a hook is holding is a person's to answer, not silence's to clear.
func staleRunning(s protocol.Session, written time.Time, idle time.Duration) bool {
	return s.State == protocol.StateRunning && s.PendingApproval == nil &&
		!s.Guess.At.IsZero() && written.After(s.Guess.At) && idle >= staleHookAfter
}

// applyMeta copies what a transcript says about its session onto the row:
// title, name, branch and model, for free, without asking the agent anything.
// Applied whether or not the chunk held any conversation, since a /rename is a
// line of its own.
func (m *Manager) applyMeta(sessionID string, meta adapter.Meta) {
	if meta.Title == "" && meta.CustomTitle == nil && meta.GitBranch == "" &&
		meta.Model == "" && meta.Version == "" && meta.Cwd == "" && meta.Queue == nil {
		return
	}
	m.reg.Update(sessionID, func(s *protocol.Session) {
		if meta.Queue != nil {
			s.Queued = append([]protocol.QueuedMessage(nil), (*meta.Queue)...)
		}
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
		if meta.Cwd != "" {
			s.Cwd = meta.Cwd
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
		if a, ok := m.adapterByID(agentID); ok && p.Path != "" {
			if _, locates := a.(adapter.Locator); locates {
				if path := m.matchTranscript(agentID, p.Path); path != "" {
					m.reg.Upsert(agentID, natural, func(t *protocol.Session) {
						t.TranscriptPath = path
					})
					m.EnsureTailer(sess.ID, path)
				}
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
// StopPersisting stops mirroring the registry into the store, once what has
// already changed is written. The store can be closed after it, and not
// before: a write still in flight would land in a closed database, or in a
// directory being removed.
func (m *Manager) StopPersisting() {
	m.persistOnce.Do(m.persistStop)
	<-m.persistDone
}

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
