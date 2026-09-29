package attach

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

// iTerm2 adoption.
//
// This is the mechanism for the case the rest of the package cannot reach: an
// agent the user started in an ordinary iTerm2 window, before muxscale existed,
// not under tmux and not under `muxscale run`. Its PTY master belongs to iTerm2
// and no other process can borrow it -- but iTerm2's scripting interface reads
// a session's screen and types into it, which is the pair of capabilities
// needed to make such a session usable from a phone.
//
// Sessions are joined to processes on the tty. A discovered agent process has
// one; every iTerm2 session reports the one it owns; the match is exact and
// needs no guessing about window titles. The iTerm2 session's own id is what
// gets stored, because a tty number is reused once the session closes and a
// stale target that still resolves is worse than one that fails.
//
// Reads are polled, so these sessions carry a *sampled* screen rather than a
// live VT stream, and say so. See SampledNotice.

// itermTimeout bounds every osascript call. iTerm2 answers in tens of
// milliseconds when healthy; a hang here would otherwise stall the discovery
// loop, which runs on a ticker and has other sessions to get to.
const itermTimeout = 5 * time.Second

// ITerm2Session is one iTerm2 session (a pane, in its terminology).
type ITerm2Session struct {
	// ID is iTerm2's own session id -- a UUID, stable for the session's life.
	ID string
	// TTY is the device the session owns, e.g. "/dev/ttys039". This is the
	// join key against a discovered process.
	TTY string
}

// listSessionsScript enumerates every session in every tab of every window.
//
// Both properties are fetched in bulk rather than by walking the object tree
// one session at a time. The walk is the obvious way to write this and is
// eighteen times slower here -- one Apple event per property per session, ~2s
// across three dozen sessions -- which is too slow to sit inside a discovery
// loop that runs every few seconds.
//
// The cost of the bulk form is that the two lists are correlated by position
// rather than by construction. They come from the same traversal so they do
// line up, but "do" is not "must", and a mispairing here would mean typing
// into the wrong window. Length equality is checked below, and ITerm2SendText
// re-reads the target's tty before writing, so a wrong pairing cannot survive
// into an actual keystroke.
const listSessionsScript = `
set AppleScript's text item delimiters to (ASCII character 10)
tell application "iTerm2"
  set ids to (id of every session of every tab of every window)
  set ttys to (tty of every session of every tab of every window)
end tell
return (ids as text) & (ASCII character 9) & (ttys as text)
`

// ITerm2Sessions lists iTerm2's open sessions.
func ITerm2Sessions() ([]ITerm2Session, error) {
	out, err := runOsascript(listSessionsScript)
	if err != nil {
		return nil, err
	}
	return parseSessionList(out)
}

func parseSessionList(out string) ([]ITerm2Session, error) {
	halves := strings.SplitN(out, "\t", 2)
	if len(halves) != 2 {
		return nil, fmt.Errorf("iTerm2 returned an unreadable session list")
	}
	ids := nonEmptyLines(halves[0])
	ttys := nonEmptyLines(halves[1])
	if len(ids) != len(ttys) {
		return nil, fmt.Errorf(
			"iTerm2 reported %d session ids and %d ttys; refusing to guess which belongs to which",
			len(ids), len(ttys))
	}
	found := make([]ITerm2Session, 0, len(ids))
	for i := range ids {
		found = append(found, ITerm2Session{ID: ids[i], TTY: ttys[i]})
	}
	return found, nil
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// ITerm2SendText types data into a session, as though the user had typed it.
//
// wantTTY is the tty the caller believes this session owns, and the write is
// refused if iTerm2 disagrees. Two things make that worth an extra property
// read: session ids are paired to ttys by list position (see
// listSessionsScript), and a cached target can outlive the session it names.
// The failure this prevents is a phone's reply appearing in whichever window
// happens to sit at that index now -- silently, in someone else's work.
//
// Nothing is appended. The caller has already decided whether the input ends
// in a carriage return, and adding one here would submit a line the user meant
// to leave sitting in the composer.
func ITerm2SendText(sessionID, wantTTY string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	script := fmt.Sprintf(`
tell application "iTerm2"
  repeat with w in windows
    repeat with t in tabs of w
      repeat with s in sessions of t
        if (id of s) is %s then
          if (tty of s) is not %s then return "moved"
          tell s to write text %s newline NO
          return "ok"
        end if
      end repeat
    end repeat
  end repeat
  return "gone"
end tell
`, quoteAppleScript(sessionID), quoteAppleScript(wantTTY), appleScriptBytes(data))

	out, err := runOsascript(script)
	if err != nil {
		return err
	}
	switch strings.TrimSpace(out) {
	case "ok":
		return nil
	case "gone":
		return fmt.Errorf("the iTerm2 window for this session has been closed")
	case "moved":
		return fmt.Errorf(
			"the iTerm2 window for this session is no longer on %s; not typing into it", wantTTY)
	}
	return fmt.Errorf("iTerm2 gave an unexpected answer: %q", out)
}

// ITerm2Text reads a session's visible screen.
func ITerm2Text(sessionID string) (string, error) {
	script := fmt.Sprintf(`
tell application "iTerm2"
  repeat with w in windows
    repeat with t in tabs of w
      repeat with s in sessions of t
        if (id of s) is %s then return (text of s)
      end repeat
    end repeat
  end repeat
  error "no such session"
end tell
`, quoteAppleScript(sessionID))
	return runOsascript(script)
}

// runOsascript runs a script read from stdin.
//
// Via stdin rather than -e so a script carrying arbitrary user text cannot run
// into argv limits or the shell's idea of quoting.
func runOsascript(script string) (string, error) {
	cmd := exec.Command("osascript", "-")
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return "", osascriptError(stderr.String(), err)
		}
	case <-time.After(itermTimeout):
		_ = cmd.Process.Kill()
		<-done
		return "", fmt.Errorf("iTerm2 did not answer within %s", itermTimeout)
	}
	// osascript terminates its result with a newline of its own, which is not
	// part of the value.
	return strings.TrimSuffix(stdout.String(), "\n"), nil
}

// osascriptError turns AppleScript's numbered failures into something a user
// can act on.
//
// -1743 is the one that matters. It means macOS has not granted this process
// permission to control iTerm2, and it is the failure every daemon not started
// from a terminal hits first. The raw message ("Not authorized to send Apple
// events") does not say where the switch is, and the switch is not anywhere
// obvious.
func osascriptError(stderr string, err error) error {
	msg := strings.TrimSpace(stderr)
	switch {
	case strings.Contains(msg, "-1743"), strings.Contains(msg, "Not authorized to send Apple events"):
		return fmt.Errorf(
			"macOS has not allowed muxscale to control iTerm2.\n" +
				"Grant it in System Settings > Privacy & Security > Automation, " +
				"under the entry for muxscale (or for the terminal the daemon was " +
				"started from), then restart the daemon")
	case strings.Contains(msg, "-600"), strings.Contains(msg, "isn’t running"), strings.Contains(msg, "isn't running"):
		return fmt.Errorf("iTerm2 is not running")
	case msg != "":
		return fmt.Errorf("iTerm2 scripting: %s", msg)
	}
	return fmt.Errorf("iTerm2 scripting: %w", err)
}

// quoteAppleScript renders a Go string as an AppleScript string literal.
func quoteAppleScript(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// appleScriptBytes renders arbitrary input as an AppleScript string expression.
//
// Printable text goes in as a quoted literal, which keeps non-ASCII correct:
// AppleScript strings are Unicode and osascript reads its source as UTF-8, so
// an emoji or an accented character survives unchanged. Control characters
// cannot appear in a literal at all and become `ASCII character N` terms
// concatenated in -- which is the whole point of this function, because the
// control characters are the interesting input. Escape, carriage return and
// Ctrl-C are how a phone answers a prompt.
func appleScriptBytes(data []byte) string {
	var parts []string
	var lit strings.Builder

	flush := func() {
		if lit.Len() > 0 {
			parts = append(parts, quoteAppleScript(lit.String()))
			lit.Reset()
		}
	}

	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			// Not valid UTF-8. Pass the byte through as-is rather than
			// substituting U+FFFD: a client that sent a raw byte meant it.
			flush()
			parts = append(parts, fmt.Sprintf("(ASCII character %d)", data[i]))
			i++
			continue
		}
		if r < 0x20 || r == 0x7f {
			flush()
			parts = append(parts, fmt.Sprintf("(ASCII character %d)", r))
		} else {
			lit.WriteRune(r)
		}
		i += size
	}
	flush()

	if len(parts) == 0 {
		return `""`
	}
	return strings.Join(parts, " & ")
}
