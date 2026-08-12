package attach

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
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
	"tmux":        "tmux",
	"tmux:server": "tmux",
	"kitty":       "kitty",
	"wezterm-gui": "wezterm",
	"wezterm":     "wezterm",
	"iTerm2":      "iterm2",
	"zellij":      "zellij",
	"screen":      "screen",
	"Terminal":    "apple-terminal",
	"ghostty":     "ghostty",
	"Code Helper": "vscode",
	"Code":        "vscode",
}

// Discover finds running agent processes.
//
// It shells out to ps rather than reading /proc so the same code path works on
// macOS and Linux. The cost is one exec every scan interval; at a 2s cadence
// that is negligible next to correctness on both platforms.
func Discover() ([]Process, error) {
	// args= rather than comm=, because comm is only argv[0] and an agent
	// installed via npm runs as `node .../@anthropic-ai/claude-code/cli.js`.
	// Under comm that process is indistinguishable from any other node
	// process, so those installs were skipped entirely by the scan.
	//
	// The command name is recovered as base(argv[0]), which yields the same
	// string comm did for every entry in AgentBinaries and terminalAncestors,
	// including the two awkward ones: VS Code's helper resolves to
	// "Code Helper" and tmux to "tmux".
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,lstart=,tty=,args=").Output()
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
		// pid ppid <lstart: 5 fields> tty args...
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
		argv := fields[8:]
		all[pid] = procRow{
			pid: pid, ppid: ppid,
			comm: filepath.Base(argv[0]), argv: argv,
			tty: tty, started: started,
		}
		order = append(order, pid)
	}

	var found []Process
	for _, pid := range order {
		r := all[pid]
		if !isAgentProcess(r) {
			continue
		}
		// Skip our own wrapped children: they are already attached at higher
		// fidelity and would otherwise appear twice.
		p := Process{
			Pid: r.pid, PPid: r.ppid, Command: r.comm, Args: r.argv,
			TTY: r.tty, Started: r.started,
		}
		p.Cwd = processCwd(r.pid)
		p.Ancestor = findAncestor(all, r.ppid)
		found = append(found, p)
	}
	return found, nil
}

// ForAdapter converts a discovered process into the form adapters match on.
//
// The conversion is narrowing on purpose: adapters get argv, cwd and ancestry
// and deliberately not the pid as anything they could act on. Detection is a
// pure question about a process, and an adapter that reaches out to poke at one
// is doing something this seam should not make easy.
func (p Process) ForAdapter() adapter.Process {
	argv := p.Args
	if len(argv) == 0 {
		// tmux panes report a command with no argv. Detect only looks at
		// argv[0] for the common case, so synthesising it keeps one detection
		// path rather than adding a name-only variant that could disagree.
		argv = []string{p.Command}
	}
	var ancestry []string
	if p.Ancestor != "" {
		ancestry = []string{p.Ancestor}
	}
	return adapter.Process{
		PID: p.Pid, PPID: p.PPid,
		Argv: argv, Cwd: p.Cwd, TTY: p.TTY,
		Ancestry: ancestry,
	}
}

// procRow is one line of ps output.
type procRow struct {
	pid, ppid int
	comm, tty string
	argv      []string
	started   time.Time
}

// isAgentProcess decides whether a process is worth reporting.
//
// The binary-name table covers agents muxscale can see but has no adapter for
// yet -- amp, cursor-agent, aider, goose, gemini. Per spec §4.3.7 the process
// scan alone still yields their session list, liveness and project, so they
// are kept here rather than dropped for lack of an adapter.
//
// The argv scan is what catches an agent whose argv[0] is its runtime rather
// than itself, which the name table by construction cannot.
func isAgentProcess(r procRow) bool {
	if _, ok := AgentBinaries[r.comm]; ok {
		return true
	}
	for _, a := range r.argv[1:] {
		for _, marker := range argvMarkers {
			if strings.Contains(a, marker) {
				return true
			}
		}
	}
	return false
}

// argvMarkers identify an agent launched through a runtime, where argv[0] is
// `node` or `python` and tells us nothing.
//
// Deliberately package-path prefixes rather than bare names: matching "codex"
// anywhere in argv would claim `grep -r codex`, and a session list containing
// the user's grep is worse than one missing an unusual install.
var argvMarkers = []string{
	"@anthropic-ai/claude-code",
	"@openai/codex",
	"@earendil-works/pi-coding-agent",
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
