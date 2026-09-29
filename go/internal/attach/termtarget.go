package attach

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// TermTarget is an adopted terminal pane: somewhere muxscale can type and
// read, owned by an emulator rather than by muxscale.
//
// Only iTerm2 is wired up. kitty and WezTerm have equivalent scripting
// interfaces and their send/read calls already exist in this package, but
// neither joins a pane to a process by tty the way iTerm2 does -- kitty
// reports foreground pids, WezTerm reports neither -- so adopting them needs a
// different join and is not done here. DetectTerminals still reports their
// capability, because `muxscale doctor` should say what a machine could do.
type TermTarget struct {
	Kind TermKind
	// ID is the emulator's own handle for the pane.
	ID string
	// TTY is what the pane owned when it was adopted. Carried so a write can
	// confirm the pane is still the one that was meant; see ITerm2SendText.
	TTY string

	// Pid and Started identify the agent process this pane was adopted for.
	//
	// They are the guard against the nastiest failure this mechanism has. When
	// the agent exits, its pane does not: the window stays open with a shell
	// back at a prompt, same pane id, same tty. Every check based on the pane
	// still passes, and a reply typed from a phone lands on a shell command
	// line -- where a stray "y" or a pasted instruction is a command. So the
	// process is checked too, and the start time along with it, because pids
	// are reused.
	Pid     int
	Started time.Time
}

// errTargetStale marks a target that no longer names a live pane, so a caller
// can drop it and let the next scan re-adopt instead of retrying forever.
var errTargetStale = errors.New("terminal pane is gone")

// IsTargetStale reports whether an error means the pane no longer exists.
func IsTargetStale(err error) bool { return errors.Is(err, errTargetStale) }

// SendText types into the pane.
//
// Refused unless the agent this pane was adopted for is still the process
// sitting in it.
func (t TermTarget) SendText(data []byte) error {
	if err := t.checkProcess(); err != nil {
		return err
	}
	switch t.Kind {
	case TermITerm2:
		err := ITerm2SendText(t.ID, t.TTY, data)
		return markStale(err)
	}
	return fmt.Errorf("muxscale cannot type into %s panes", t.Kind)
}

// Text reads the pane's visible screen.
func (t TermTarget) Text() (string, error) {
	switch t.Kind {
	case TermITerm2:
		s, err := ITerm2Text(t.ID)
		return s, markStale(err)
	}
	return "", fmt.Errorf("muxscale cannot read %s panes", t.Kind)
}

// checkProcess confirms the adopted agent is still alive and still the same
// process.
//
// Start time is compared, not just liveness: a pid that has been recycled onto
// something else passes a liveness check and is exactly the case worth
// catching. A target that fails here is stale, so the caller drops it and the
// next scan re-adopts if an agent is really there.
func (t TermTarget) checkProcess() error {
	if t.Pid == 0 {
		return nil // adopted without process information; nothing to check
	}
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(t.Pid)).Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return fmt.Errorf("%w: the agent on %s has exited", errTargetStale, t.TTY)
	}
	if t.Started.IsZero() {
		return nil
	}
	started := ParseLstart(string(out))
	if started.IsZero() {
		// Unreadable is not proof of anything; liveness already passed.
		return nil
	}
	if started.Sub(t.Started).Abs() > 2*time.Second {
		return fmt.Errorf("%w: process %d is no longer the agent muxscale adopted", errTargetStale, t.Pid)
	}
	return nil
}

// markStale tags the errors that mean "re-adopt me", leaving everything else
// -- a denied Automation permission, a hung emulator -- alone. Those are
// conditions to report, not targets to discard.
func markStale(err error) error {
	if err == nil {
		return nil
	}
	m := err.Error()
	if strings.Contains(m, "has been closed") ||
		strings.Contains(m, "no longer on") ||
		strings.Contains(m, "no such session") {
		return fmt.Errorf("%w: %s", errTargetStale, m)
	}
	return err
}

// DevPath turns a tty as `ps` reports it into the absolute path a terminal
// emulator reports. `ps` prints "ttys039"; iTerm2 says "/dev/ttys039".
// An empty or detached tty ("??", "-") has no path and yields "".
func DevPath(tty string) string {
	switch tty {
	case "", "?", "??", "-":
		return ""
	}
	if strings.HasPrefix(tty, "/dev/") {
		return tty
	}
	return "/dev/" + tty
}

// AdoptableTerminalPanes maps tty to pane for every pane muxscale could adopt.
//
// The tty is the join key because it is the one fact a discovered process and
// a terminal pane both know, exactly and independently. Titles are guesses and
// working directories are shared by half a dozen windows at a time.
func AdoptableTerminalPanes() (map[string]TermTarget, error) {
	sessions, err := ITerm2Sessions()
	if err != nil {
		return nil, err
	}
	out := make(map[string]TermTarget, len(sessions))
	for _, s := range sessions {
		if s.TTY == "" || !strings.HasPrefix(s.TTY, "/dev/") {
			continue // a session with no pane of its own, e.g. one still starting
		}
		out[s.TTY] = TermTarget{Kind: TermITerm2, ID: s.ID, TTY: s.TTY}
	}
	return out, nil
}
