package attach

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These tests drive a real tmux server on a private socket so they never touch
// the user's own sessions.
const testSocket = "muxscale-test"

func requireTmux(t *testing.T) {
	t.Helper()
	if !TmuxAvailable() {
		t.Skip("tmux not installed")
	}
}

func tmuxTest(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	return exec.Command("tmux", append([]string{"-L", testSocket}, args...)...)
}

func startTestServer(t *testing.T, session, command string) {
	t.Helper()
	if err := tmuxTest(t, "new-session", "-d", "-s", session, command).Run(); err != nil {
		t.Skipf("could not start a tmux test server: %v", err)
	}
	t.Cleanup(func() { _ = tmuxTest(t, "kill-server").Run() })
	time.Sleep(500 * time.Millisecond)
}

func TestTmuxVersionParses(t *testing.T) {
	requireTmux(t)
	v, err := TmuxVersion()
	if err != nil {
		t.Fatalf("TmuxVersion: %v", err)
	}
	if !strings.HasPrefix(v, "tmux ") {
		t.Errorf("version = %q, want a leading \"tmux \"", v)
	}
	t.Logf("tmux version: %s", v)
}

// No tmux server running is a normal state, not an error worth surfacing to a
// user who simply does not use tmux.
func TestListPanesWithNoServerIsNotAnError(t *testing.T) {
	requireTmux(t)
	panes, err := ListPanes()
	if err != nil {
		t.Fatalf("ListPanes on the default socket errored: %v", err)
	}
	t.Logf("found %d panes on the user's default tmux socket", len(panes))
}

// Input must reach the pane's process byte-for-byte. Hex encoding exists
// precisely so tmux does not reinterpret what muxscale already decided.
func TestSendKeysDeliversExactBytes(t *testing.T) {
	requireTmux(t)
	startTestServer(t, "s1", "sh -c 'while read line; do echo GOT:$line; done'")

	payload := EncodeText("hello tmux", true)
	args := []string{"-L", testSocket, "send-keys", "-H", "-t", "s1"}
	for _, b := range payload {
		args = append(args, string([]byte{hexDigit(b >> 4), hexDigit(b & 0xf)}))
	}
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		t.Fatalf("send-keys: %v: %s", err, out)
	}
	time.Sleep(600 * time.Millisecond)

	out, err := exec.Command("tmux", "-L", testSocket, "capture-pane", "-p", "-t", "s1").Output()
	if err != nil {
		t.Fatalf("capture-pane: %v", err)
	}
	if !strings.Contains(string(out), "GOT:hello tmux") {
		t.Errorf("input did not reach the pane's process; pane contents:\n%s", out)
	}
}

func TestCapturePaneReturnsContent(t *testing.T) {
	requireTmux(t)
	startTestServer(t, "s2", "sh -c 'echo distinctive-marker; sleep 30'")

	out, err := exec.Command("tmux", "-L", testSocket, "capture-pane", "-p", "-e", "-t", "s2").Output()
	if err != nil {
		t.Fatalf("capture-pane: %v", err)
	}
	if !strings.Contains(string(out), "distinctive-marker") {
		t.Errorf("capture missed the pane's content:\n%s", out)
	}
}

func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	got := shellQuote("/tmp/it's here/fifo")
	// The result must be a single shell word that expands back to the original.
	out, err := exec.Command("sh", "-c", "printf %s "+got).Output()
	if err != nil {
		t.Fatalf("quoting produced an unusable word %q: %v", got, err)
	}
	if string(out) != "/tmp/it's here/fifo" {
		t.Errorf("round trip = %q", out)
	}
}

func hexDigit(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'a' + n - 10
}

// tmux reports "nothing is running" in more than one wording, and matching only
// one of them made a machine with no tmux server look like a broken one.
func TestNoTmuxServerRecognisesBothWordings(t *testing.T) {
	yes := []string{
		"no server running on /private/tmp/tmux-501/default",
		"error connecting to /private/tmp/tmux-501/default (No such file or directory)",
		"error connecting to /tmp/tmux-0/default (no such file or directory)",
	}
	for _, s := range yes {
		if !noTmuxServer(s) {
			t.Errorf("not recognised as an absent server: %q", s)
		}
	}

	// Real failures must still surface: swallowing these would make a broken
	// tmux indistinguishable from an unused one.
	no := []string{
		"error connecting to /tmp/tmux-501/default (Permission denied)",
		"lost server",
		"",
		"unknown command: list-panes",
	}
	for _, s := range no {
		if noTmuxServer(s) {
			t.Errorf("wrongly treated as an absent server: %q", s)
		}
	}
}
