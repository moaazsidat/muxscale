package attach

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
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
	Kind      TermKind
	Available bool   // the control binary exists
	Enabled   bool   // remote control actually works right now
	Reason    string // why not, and what to do about it
	HowToFix  string
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
	// `kitty @ ls` is the cheapest call that proves remote control is on.
	if err := exec.Command("kitty", "@", "ls").Run(); err != nil {
		c.Reason = "kitty remote control is disabled"
		c.HowToFix = "add `allow_remote_control yes` and `listen_on unix:/tmp/kitty` to kitty.conf, then restart kitty"
		return c
	}
	c.Enabled = true
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

func detectITerm2() TermCapability {
	c := TermCapability{Kind: TermITerm2}
	// iTerm2's Python API needs its own runtime; AppleScript is the fallback
	// and needs Automation permission, which cannot be probed without
	// triggering a permission prompt. Report availability only.
	if _, err := os.Stat("/Applications/iTerm.app"); err != nil {
		c.Reason = "iTerm2 is not installed"
		return c
	}
	c.Available = true
	c.Reason = "iTerm2 control needs the Python API enabled and Automation permission"
	c.HowToFix = "enable the Python API in iTerm2 > Settings > General > Magic"
	return c
}

// KittyPane is a kitty window that may hold an agent.
type KittyPane struct {
	ID      int
	Title   string
	Cwd     string
	Command string
}

// KittyWindows lists kitty windows via its remote-control API.
func KittyWindows() ([]KittyPane, error) {
	out, err := exec.Command("kitty", "@", "ls").Output()
	if err != nil {
		return nil, fmt.Errorf("kitty @ ls: %w", err)
	}
	return parseKittyLS(out)
}

// KittySendText injects text into a kitty window.
func KittySendText(windowID int, data []byte) error {
	cmd := exec.Command("kitty", "@", "send-text",
		"--match", fmt.Sprintf("id:%d", windowID), "--stdin")
	cmd.Stdin = strings.NewReader(string(data))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("kitty send-text: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// KittyGetText reads a kitty window's contents.
func KittyGetText(windowID int, includeScrollback bool) ([]byte, error) {
	args := []string{"@", "get-text", "--match", fmt.Sprintf("id:%d", windowID)}
	if includeScrollback {
		args = append(args, "--extent", "all")
	}
	out, err := exec.Command("kitty", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("kitty get-text: %w", err)
	}
	return out, nil
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
