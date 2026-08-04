package attach

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// tmux adoption.
//
// Most people already run agents in tmux, so this is the mechanism that makes
// an already-running session writable without asking anyone to change habits.
// It is an adoption path, not a foundation: pane size is shared, pipe-pane has
// no backpressure or sequence numbers, and history from capture-pane is
// re-rendered text rather than the original byte stream. An owned PTY is
// strictly better when muxscale launched the process.

// TmuxPane is a discovered tmux pane.
type TmuxPane struct {
	Target  string // session:window.pane, stable enough to address
	PaneID  string // %N, stable across renumbering -- prefer this
	Pid     int
	Command string
	Path    string
	Width   int
	Height  int
	Active  bool
}

// TmuxAvailable reports whether a usable tmux is installed.
func TmuxAvailable() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// TmuxVersion returns the installed tmux version string.
func TmuxVersion() (string, error) {
	out, err := exec.Command("tmux", "-V").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ListPanes enumerates every pane on the running tmux server.
func ListPanes() ([]TmuxPane, error) {
	const format = "#{session_name}:#{window_index}.#{pane_index}\t#{pane_id}\t" +
		"#{pane_pid}\t#{pane_current_command}\t#{pane_current_path}\t" +
		"#{pane_width}\t#{pane_height}\t#{pane_active}"

	out, err := exec.Command("tmux", "list-panes", "-a", "-F", format).Output()
	if err != nil {
		// No server running is a normal state, not an error worth surfacing.
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 &&
			strings.Contains(string(ee.Stderr), "no server running") {
			return nil, nil
		}
		return nil, err
	}

	var panes []TmuxPane
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 8 {
			continue
		}
		pid, _ := strconv.Atoi(f[2])
		w, _ := strconv.Atoi(f[5])
		h, _ := strconv.Atoi(f[6])
		panes = append(panes, TmuxPane{
			Target: f[0], PaneID: f[1], Pid: pid, Command: f[3],
			Path: f[4], Width: w, Height: h, Active: f[7] == "1",
		})
	}
	return panes, nil
}

// FindAgentPanes returns panes whose foreground process is a known agent.
func FindAgentPanes() ([]TmuxPane, error) {
	panes, err := ListPanes()
	if err != nil {
		return nil, err
	}
	var out []TmuxPane
	for _, p := range panes {
		if _, ok := AgentBinaries[p.Command]; ok {
			out = append(out, p)
			continue
		}
		// The agent may be a child of a shell in the pane, so also check
		// whether any descendant matches.
		if agent := agentInSubtree(p.Pid); agent != "" {
			p.Command = agent
			out = append(out, p)
		}
	}
	return out, nil
}

// agentInSubtree looks for a known agent binary among a pane's descendants.
func agentInSubtree(rootPid int) string {
	procs, err := Discover()
	if err != nil {
		return ""
	}
	for _, p := range procs {
		if isDescendantOf(p.Pid, rootPid) {
			return p.Command
		}
	}
	return ""
}

func isDescendantOf(pid, ancestor int) bool {
	for depth := 0; depth < 12 && pid > 1; depth++ {
		out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return false
		}
		ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			return false
		}
		if ppid == ancestor {
			return true
		}
		pid = ppid
	}
	return false
}

// CapturePane returns the pane's visible content plus history, with escape
// sequences preserved.
//
// Note this is tmux's *re-rendered* text, not the original byte stream, so it
// is a different dialect from what pipe-pane delivers live. Seeding history
// from here and then streaming live output means the emulator sees both;
// subtle scrollback artefacts live in that seam, which is one reason tmux is
// an adoption path rather than the foundation.
func CapturePane(target string, historyLines int) ([]byte, error) {
	args := []string{"capture-pane", "-p", "-e", "-t", target}
	if historyLines > 0 {
		args = append(args, "-S", "-"+strconv.Itoa(historyLines))
	}
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("capture-pane %s: %w", target, err)
	}
	return out, nil
}

// SendKeys sends literal bytes to a pane.
//
// Hex encoding (-H) is used rather than letting tmux interpret key names,
// because muxscale has already decided the exact bytes and a second layer of
// interpretation is how a newline turns into something else entirely.
func SendKeys(target string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	args := []string{"send-keys", "-H", "-t", target}
	for _, b := range data {
		args = append(args, fmt.Sprintf("%02x", b))
	}
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("send-keys to %s: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// PipePaneCommand returns the shell command that streams a pane's output to a
// FIFO. `-o` only captures output produced *after* it is enabled, which is why
// history has to be seeded separately with CapturePane.
func PipePaneCommand(target, fifo string) *exec.Cmd {
	return exec.Command("tmux", "pipe-pane", "-o", "-t", target,
		fmt.Sprintf("cat >> %s", shellQuote(fifo)))
}

// StopPipePane disables output piping for a pane.
func StopPipePane(target string) error {
	return exec.Command("tmux", "pipe-pane", "-t", target).Run()
}

// PaneExists reports whether a pane is still present.
func PaneExists(target string) bool {
	err := exec.Command("tmux", "has-session", "-t",
		strings.SplitN(target, ":", 2)[0]).Run()
	if err != nil {
		return false
	}
	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_id}").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), target) ||
		paneTargetResolves(target)
}

func paneTargetResolves(target string) bool {
	return exec.Command("tmux", "display-message", "-p", "-t", target, "#{pane_id}").Run() == nil
}

// shellQuote makes a path safe inside the shell command tmux runs.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
