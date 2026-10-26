package attach

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// termctl adopts sessions through a terminal emulator's own remote-control API.
//
// This covers the case the PTY cannot: the user already had an agent running in
// a normal terminal window, started before muxscale existed, not in tmux. The
// PTY master belongs to the terminal emulator and no process can read another
// process's file descriptors -- but several emulators expose a sanctioned API
// that reads pane text and injects keystrokes, which is exactly the pair of
// capabilities needed.
//
// Reads are polled, not streamed, so these sessions get a *sampled* grid rather
// than a true VT stream. That is surfaced to clients rather than papered over:
// pretending a 2 Hz sample is live would be a lie the UI cannot recover from.

// TermKind identifies a supported terminal.
type TermKind string

const (
	TermKitty   TermKind = "kitty"
	TermWezTerm TermKind = "wezterm"
	TermITerm2  TermKind = "iterm2"
)

// TermCapability describes what muxscale can do with a terminal.
type TermCapability struct {
	Kind      TermKind `json:"kind"`
	Available bool     `json:"available"`        // the control binary exists
	Enabled   bool     `json:"enabled"`          // remote control actually works right now
	Reason    string   `json:"reason,omitempty"` // why not, and what to do about it
	HowToFix  string   `json:"how_to_fix,omitempty"`
}

// DetectTerminals reports remote-control support for each known terminal.
func DetectTerminals() []TermCapability {
	return []TermCapability{detectKitty(), detectWezTerm(), detectITerm2()}
}

func detectKitty() TermCapability {
	c := TermCapability{Kind: TermKitty}
	if _, err := exec.LookPath("kitty"); err != nil {
		c.Reason = "kitty is not installed"
		return c
	}
	c.Available = true
	// `kitty @ ls` is the cheapest call that proves remote control is on. It
	// goes to a socket by name: the daemon usually runs under a service
	// manager, outside any kitty window, where a bare `kitty @` has nothing
	// to talk to.
	for _, sock := range KittySockets() {
		if _, err := kittyCmd(sock, "ls"); err == nil {
			c.Enabled = true
			return c
		}
	}
	c.Reason = "kitty remote control is disabled, or no kitty is running with a socket muxscale can find"
	c.HowToFix = "add `allow_remote_control yes` and `listen_on unix:/tmp/kitty` to kitty.conf, then restart kitty"
	return c
}

func detectWezTerm() TermCapability {
	c := TermCapability{Kind: TermWezTerm}
	if _, err := exec.LookPath("wezterm"); err != nil {
		c.Reason = "wezterm is not installed"
		return c
	}
	c.Available = true
	if err := exec.Command("wezterm", "cli", "list").Run(); err != nil {
		c.Reason = "wezterm CLI could not reach a running instance"
		c.HowToFix = "start wezterm, or check that the wezterm mux server is running"
		return c
	}
	c.Enabled = true
	return c
}

// iTermInstalled looks where macOS installs apps: /Applications, or the
// user's own ~/Applications for an install made without admin rights.
func iTermInstalled() bool {
	dirs := []string{"/Applications"}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "Applications"))
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, "iTerm.app")); err == nil {
			return true
		}
	}
	return false
}

func detectITerm2() TermCapability {
	c := TermCapability{Kind: TermITerm2}
	if !iTermInstalled() {
		c.Reason = "iTerm2 is not installed"
		return c
	}
	c.Available = true

	// Whether macOS lets this process script iTerm2, asked without a prompt.
	// It used to probe with an Apple event, whose first one raised the
	// Automation prompt -- from the daemon, a background service, where it sat
	// unanswered with the event parked behind it. The prompt is `muxscale
	// up`'s to raise now (RequestITerm2Automation).
	a, err := ITerm2Automation()
	switch {
	case err != nil:
		c.Reason = err.Error()
	case a == AutomationAllowed:
		c.Enabled = true
	case a == AutomationNotRunning:
		c.Reason = "iTerm2 is not running"
		c.HowToFix = "open iTerm2; muxscale picks up its sessions within a few seconds"
	case a == AutomationNotAsked:
		c.Reason = "muxscale has not been allowed to control iTerm2 yet"
		c.HowToFix = "run `muxscale up` and click OK when macOS asks"
	default:
		c.Reason = "macOS has not allowed muxscale to control iTerm2"
		c.HowToFix = "turn it on in System Settings > Privacy & Security > Automation, under muxscale"
	}
	return c
}

// KittyPane is a kitty window that may hold an agent.
type KittyPane struct {
	ID      int
	Title   string
	Cwd     string
	Command string
	// Pid is the process kitty started in the window, usually a shell. Its
	// tty is the window's, which is how a window is joined to an agent.
	Pid int
}

// kittyTimeout bounds a remote-control call, so a hung kitty stalls one scan
// rather than the daemon.
const kittyTimeout = 3 * time.Second

// kittyCmd runs `kitty @ --to sock args...`.
func kittyCmd(sock string, args ...string) ([]byte, error) {
	return kittyCmdInput(sock, nil, args...)
}

func kittyCmdInput(sock string, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kittyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kitty", append([]string{"@", "--to", sock}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("kitty did not answer within %s", kittyTimeout)
		}
		return nil, fmt.Errorf("kitty @ %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// KittySockets lists the remote-control sockets of running kitty instances.
//
// A daemon started by a service manager does not inherit KITTY_LISTEN_ON, and
// kitty appends its own pid to a `listen_on` path from kitty.conf, so the
// sockets are found from the configured path: /tmp/kitty in the config is
// /tmp/kitty-417300 on disk, one per kitty instance.
func KittySockets() []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(os.Getenv("KITTY_LISTEN_ON"))
	listen := kittyConfListenOn()
	switch {
	case listen == "" || listen == "none":
	case strings.HasPrefix(listen, "unix:@"):
		for _, name := range abstractSockets(kittySocketPattern(strings.TrimPrefix(listen, "unix:"))) {
			add("unix:" + name)
		}
	case strings.HasPrefix(listen, "unix:"):
		path := os.ExpandEnv(strings.TrimPrefix(listen, "unix:"))
		if strings.HasPrefix(path, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				path = filepath.Join(home, path[2:])
			}
		}
		matches, _ := filepath.Glob(kittySocketPattern(path))
		if !strings.Contains(path, "{kitty_pid}") {
			matches = append([]string{path}, matches...)
		}
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && fi.Mode()&os.ModeSocket != 0 {
				add("unix:" + m)
			}
		}
	default:
		add(listen) // tcp: kitty uses it as given
	}
	return out
}

// kittySocketPattern turns a configured socket path into a glob for the ones
// kitty actually creates: {kitty_pid} stands for the pid, and a path without
// it gets -<pid> appended.
func kittySocketPattern(path string) string {
	if strings.Contains(path, "{kitty_pid}") {
		return strings.ReplaceAll(path, "{kitty_pid}", "*")
	}
	return path + "-*"
}

// abstractSockets lists Linux abstract-namespace sockets matching a glob,
// written with their leading @.
func abstractSockets(pattern string) []string {
	data, err := os.ReadFile("/proc/net/unix")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 8 {
			continue
		}
		if ok, _ := filepath.Match(pattern, f[len(f)-1]); ok {
			out = append(out, f[len(f)-1])
		}
	}
	return out
}

// kittyConfListenOn is the listen_on value from kitty.conf; the last one wins,
// as in kitty.
func kittyConfListenOn() string {
	dir := os.Getenv("KITTY_CONFIG_DIRECTORY")
	if dir == "" {
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			dir = filepath.Join(x, "kitty")
		} else if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".config", "kitty")
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "kitty.conf"))
	if err != nil {
		return ""
	}
	return parseListenOn(string(data))
}

func parseListenOn(conf string) string {
	listen := ""
	for _, line := range strings.Split(conf, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "listen_on" {
			listen = f[1]
		}
	}
	return listen
}

// KittyWindows lists the windows of the kitty listening on sock.
func KittyWindows(sock string) ([]KittyPane, error) {
	out, err := kittyCmd(sock, "ls")
	if err != nil {
		return nil, err
	}
	return parseKittyLS(out)
}

// KittySendText types data into a kitty window. Nothing is appended; see
// ITerm2SendText.
func KittySendText(sock string, windowID int, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	_, err := kittyCmdInput(sock, data, "send-text", "--match", fmt.Sprintf("id:%d", windowID), "--stdin")
	return err
}

// KittyGetText reads a kitty window's contents.
func KittyGetText(sock string, windowID int, includeScrollback bool) ([]byte, error) {
	args := []string{"get-text", "--match", fmt.Sprintf("id:%d", windowID)}
	if includeScrollback {
		args = append(args, "--extent", "all")
	}
	return kittyCmd(sock, args...)
}

// WezTermPane is a wezterm pane.
type WezTermPane struct {
	PaneID  int
	Title   string
	Cwd     string
	Command string
}

// WezTermSendText injects text into a wezterm pane.
func WezTermSendText(paneID int, data []byte) error {
	cmd := exec.Command("wezterm", "cli", "send-text", "--pane-id",
		fmt.Sprint(paneID), "--no-paste")
	cmd.Stdin = strings.NewReader(string(data))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("wezterm send-text: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// WezTermGetText reads a wezterm pane's contents.
func WezTermGetText(paneID int) ([]byte, error) {
	out, err := exec.Command("wezterm", "cli", "get-text", "--pane-id", fmt.Sprint(paneID)).Output()
	if err != nil {
		return nil, fmt.Errorf("wezterm get-text: %w", err)
	}
	return out, nil
}

// SampledNotice is shown for termctl sessions. Polling is not a live stream,
// and the UI must say so rather than implying otherwise.
const SampledNotice = "sampled at ~2 Hz via terminal remote control, not a live stream"
