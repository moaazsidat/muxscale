package daemon

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/attach"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// An agent started by hand in a shell muxscale opened -- the phone's Shell
// tab, then `claude` -- is a session of its own, found by the process scan
// and joined to its hooks, but its process is a child of a pty muxscale
// owns. Closing it stops that agent and nothing else: the shell, its
// scrollback and whatever else runs in it stay, and the prompt comes back,
// as it does after quitting the agent at a keyboard.
//
// An agent in a terminal muxscale only types into -- tmux, kitty -- is not
// closed from here. Its terminal is someone's window, and ending things in
// it from a phone is a step too far to take on a tap.

// errNotOurs says a session runs somewhere muxscale does not own.
var errNotOurs = errors.New("not ours")

// stopGrace is how long an agent has to exit on SIGTERM -- saving its
// history, putting the terminal back -- before it is killed.
const stopGrace = 5 * time.Second

func (m *Manager) stopAgentInShell(sessionID string) error {
	sess, ok := m.reg.Get(sessionID)
	if !ok {
		return fmt.Errorf("unknown session %s", sessionID)
	}
	if sess.State.Terminal() {
		return fmt.Errorf("session %s has already ended", sessionID)
	}
	pid, shellID := m.agentInShell(sess)
	if pid == 0 {
		return fmt.Errorf("session %s runs in a terminal muxscale does not own; close it there: %w", sessionID, errNotOurs)
	}

	m.log.Info("stopping an agent in a muxscale shell", "session", sessionID, "pid", pid, "shell", shellID)
	_ = m.store.Audit("human", "session_killed", sessionID, "pid="+strconv.Itoa(pid)+" shell="+shellID)
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("stopping %s: %w", sessionID, err)
	}
	go func() {
		deadline := time.Now().Add(stopGrace)
		for time.Now().Before(deadline) {
			if !alive(pid) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if alive(pid) {
			m.log.Info("agent ignored SIGTERM; killing it", "session", sessionID, "pid", pid)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		// Said now rather than at the next scan or hook, which for a session
		// joined to its hooks may never come: an agent stopped by signal
		// does not always run its own exit hook.
		m.reg.SetState(sessionID, protocol.StateGuess{
			State: protocol.StateExited, Confidence: 1, Reason: "closed",
			Evidence: "signal pid=" + strconv.Itoa(pid), At: time.Now(),
		})
	}()
	return nil
}

// agentInShell finds the process behind a session, if it runs inside a pty
// muxscale owns, and names that pty's session. Zero when it does not, or
// when which process is the session's cannot be told for certain.
func (m *Manager) agentInShell(sess protocol.Session) (pid int, shellID string) {
	shells := map[int]string{}
	m.mu.RLock()
	for id, w := range m.wrapped {
		if p := w.Pid(); p > 0 {
			shells[p] = id
		}
	}
	m.mu.RUnlock()
	if len(shells) == 0 {
		return 0, ""
	}

	procs, err := m.discover()
	if err != nil {
		return 0, ""
	}
	candidate := 0
	if n, ok := strings.CutPrefix(sess.NaturalKey, "pid:"); ok {
		candidate, _ = strconv.Atoi(n)
	}
	for _, p := range procs {
		if candidate != 0 {
			if p.Pid == candidate {
				break
			}
			continue
		}
		// A row joined to its hooks has lost the pid it was found by; the
		// transcript the process itself says is its own picks it out.
		if sess.TranscriptPath == "" || m.identify(p.Command, p.Args) != sess.AgentID {
			continue
		}
		a, ok := m.adapterByID(sess.AgentID)
		if !ok {
			continue
		}
		if loc, ok := a.(adapter.Locator); ok && loc.TranscriptForProcess(p.ForAdapter()) == sess.TranscriptPath {
			candidate = p.Pid
			break
		}
	}
	if candidate == 0 || !running(procs, candidate) {
		return 0, ""
	}
	// Up the process tree to a shell of ours, or out of it.
	for p, hops := candidate, 0; p > 1 && hops < 32; p, hops = m.parentOf(p), hops+1 {
		if id, ok := shells[p]; ok && p != candidate {
			return candidate, id
		}
	}
	return 0, ""
}

func running(procs []attach.Process, pid int) bool {
	for _, p := range procs {
		if p.Pid == pid {
			return true
		}
	}
	return false
}

// parentPid reads a process's parent from ps; 0 when it has gone.
func parentPid(pid int) int {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
