package attach

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Process-scan discovery.
//
// This is the mechanism that needs no cooperation from however the agent was
// launched: enumerate processes matching an adapter's binary patterns and read
// their cwd. It yields the session list, liveness, uptime, and project for
// every agent on the machine -- wrapped or not, started before muxscale or
// after. It cannot read output or send input; that is what the other
// attachment mechanisms are for.

// Process is a discovered agent process.
type Process struct {
	Pid     int       `json:"pid"`
	PPid    int       `json:"ppid"`
	Command string    `json:"command"`
	Args    []string  `json:"args"`
	Cwd     string    `json:"cwd"`
	TTY     string    `json:"tty"`
	Started time.Time `json:"started"`
	// Ancestor is the nearest known terminal or multiplexer, which determines
	// whether a higher-fidelity attachment is available.
	Ancestor string `json:"ancestor,omitempty"`
}

// AgentBinaries maps a binary name to an adapter id.
var AgentBinaries = map[string]string{
	"claude":       "claude-code",
	"codex":        "codex",
	"opencode":     "opencode",
	"pi":           "pi",
	"hermes":       "hermes",
	"amp":          "amp",
	"cursor-agent": "cursor-agent",
	"aider":        "aider",
	"goose":        "goose",
	"gemini":       "gemini-cli",
}

// terminalAncestors are processes that, if they own an agent, imply a
// higher-fidelity attachment may be possible.
var terminalAncestors = map[string]string{
	"tmux":         "tmux",
	"tmux:server":  "tmux",
	"kitty":        "kitty",
	"wezterm-gui":  "wezterm",
	"wezterm":      "wezterm",
	"iTerm2":       "iterm2",
	"zellij":       "zellij",
	"screen":       "screen",
	"Terminal":     "apple-terminal",
	"ghostty":      "ghostty",
	"Code Helper":  "vscode",
	"Code":         "vscode",
}

// Discover finds running agent processes.
//
// It shells out to ps rather than reading /proc so the same code path works on
// macOS and Linux. The cost is one exec every scan interval; at a 2s cadence
// that is negligible next to correctness on both platforms.
func Discover() ([]Process, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,lstart=,tty=,comm=").Output()
	if err != nil {
		return nil, err
	}

	all := map[int]procRow{}
	var order []int

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		// pid ppid <lstart: 5 fields> tty comm...
		if len(fields) < 9 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		started, _ := time.Parse("Jan 2 15:04:05 2006", strings.Join(fields[2:7], " "))
		tty := fields[7]
		comm := strings.Join(fields[8:], " ")
		all[pid] = procRow{pid: pid, ppid: ppid, comm: comm, tty: tty, started: started}
		order = append(order, pid)
	}

	var found []Process
	for _, pid := range order {
		r := all[pid]
		base := filepath.Base(r.comm)
		if _, ok := AgentBinaries[base]; !ok {
			continue
		}
		// Skip our own wrapped children: they are already attached at higher
		// fidelity and would otherwise appear twice.
		p := Process{
			Pid: r.pid, PPid: r.ppid, Command: base,
			TTY: r.tty, Started: r.started,
		}
		p.Cwd = processCwd(r.pid)
		p.Ancestor = findAncestor(all, r.ppid)
		found = append(found, p)
	}
	return found, nil
}

// procRow is one line of ps output.
type procRow struct {
	pid, ppid int
	comm, tty string
	started   time.Time
}

// findAncestor walks up the process tree looking for a known terminal or
// multiplexer, which tells us whether a writable attachment is available.
func findAncestor(all map[int]procRow, ppid int) string {
	for depth := 0; depth < 12 && ppid > 1; depth++ {
		parent, ok := all[ppid]
		if !ok {
			return ""
		}
		base := filepath.Base(parent.comm)
		if kind, ok := terminalAncestors[base]; ok {
			return kind
		}
		ppid = parent.ppid
	}
	return ""
}

// processCwd reads a process's working directory.
func processCwd(pid int) string {
	// Linux: /proc is authoritative and cheap.
	if link, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd"); err == nil {
		return link
	}
	// macOS: lsof is the available route. It is slow, so this is only called
	// for processes that already matched an agent binary.
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n/") {
			return strings.TrimPrefix(line, "n")
		}
	}
	return ""
}

// WritableVia reports how input could reach this process, or "" if it cannot.
// This drives the capability bits a client uses to decide whether to show a
// compose box -- an input affordance that silently fails is worse than none.
func (p Process) WritableVia() string {
	switch p.Ancestor {
	case "tmux", "zellij", "screen":
		return p.Ancestor
	case "kitty", "wezterm", "iterm2":
		return "termctl:" + p.Ancestor
	default:
		return ""
	}
}
