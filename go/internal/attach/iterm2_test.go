package attach

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Control characters are the point of this encoder. Escape, carriage return
// and Ctrl-C are how a phone answers a prompt, and none of them can appear in
// an AppleScript string literal.
func TestAppleScriptBytesEncodesControlCharacters(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"plain text", []byte("yes"), `"yes"`},
		{"carriage return alone", []byte("\r"), `(ASCII character 13)`},
		{"text then submit", []byte("ship it\r"), `"ship it" & (ASCII character 13)`},
		{"escape", []byte("\x1b"), `(ASCII character 27)`},
		{"arrow key", []byte("\x1b[B"), `(ASCII character 27) & "[B"`},
		{"ctrl-c", []byte{3}, `(ASCII character 3)`},
		{"empty", nil, `""`},
		// A quote or backslash in the text must not end the literal early --
		// this is the injection that would let a reply run AppleScript.
		{"quote", []byte(`say "hi"`), `"say \"hi\""`},
		{"backslash", []byte(`C:\tmp`), `"C:\\tmp"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := appleScriptBytes(tc.in); got != tc.want {
				t.Errorf("appleScriptBytes(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// Non-ASCII must survive as itself. Encoding it byte-by-byte as ASCII
// characters would deliver mojibake to the agent.
func TestAppleScriptBytesKeepsUnicodeIntact(t *testing.T) {
	got := appleScriptBytes([]byte("héllo 🎉"))
	if got != `"héllo 🎉"` {
		t.Errorf("got %s", got)
	}
}

// A stray non-UTF-8 byte must not become U+FFFD: a client that sent a raw byte
// meant that byte.
func TestAppleScriptBytesPassesInvalidUTF8Through(t *testing.T) {
	got := appleScriptBytes([]byte{'a', 0xff, 'b'})
	if got != `"a" & (ASCII character 255) & "b"` {
		t.Errorf("got %s", got)
	}
}

// The two bulk lists are correlated by position, so a length mismatch means
// the correlation is unsafe. Guessing here would type into the wrong window.
func TestParseSessionListRefusesAMismatch(t *testing.T) {
	_, err := parseSessionList("id-1\nid-2\t/dev/ttys001")
	if err == nil {
		t.Fatal("a mismatched list was accepted")
	}
	if !strings.Contains(err.Error(), "refusing to guess") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestParseSessionListPairsByPosition(t *testing.T) {
	got, err := parseSessionList("id-1\nid-2\n\t/dev/ttys001\n/dev/ttys002\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []ITerm2Session{
		{ID: "id-1", TTY: "/dev/ttys001"},
		{ID: "id-2", TTY: "/dev/ttys002"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d sessions, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("session %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// An output with no separator at all is a failure, not an empty tailnet of
// sessions -- reporting "no panes" would quietly disable adoption.
func TestParseSessionListRejectsGarbage(t *testing.T) {
	if _, err := parseSessionList("something went wrong"); err == nil {
		t.Fatal("garbage was read as an empty session list")
	}
}

// The permission failure is the one every daemon not started from a terminal
// hits, and the raw message does not say where the switch is.
func TestOsascriptErrorNamesTheAutomationSetting(t *testing.T) {
	err := osascriptError("execution error: Not authorized to send Apple events to iTerm2. (-1743)", nil)
	if !strings.Contains(err.Error(), "Privacy & Security > Automation") {
		t.Errorf("does not say where to fix it: %v", err)
	}
}

// Both orderings of ps's lstart appear on macOS depending on locale. Parsing
// only one left every discovered process with a zero start time.
func TestParseLstartHandlesBothLocaleOrderings(t *testing.T) {
	want := time.Date(2026, time.September, 25, 21, 2, 16, 0, time.Local)
	for _, in := range []string{
		"Fri Sep 25 21:02:16 2026",
		"Fri 25 Sep 21:02:16 2026",
		"Fri Sep  5 21:02:16 2026", // ps pads single-digit days
	} {
		got := ParseLstart(in)
		if got.IsZero() {
			t.Errorf("ParseLstart(%q) failed to parse", in)
			continue
		}
		if in == "Fri Sep  5 21:02:16 2026" {
			continue
		}
		if !got.Equal(want) {
			t.Errorf("ParseLstart(%q) = %v, want %v", in, got, want)
		}
	}
	if !ParseLstart("not a time").IsZero() {
		t.Error("garbage parsed as a time")
	}
}

func TestDevPath(t *testing.T) {
	cases := map[string]string{
		"ttys039":      "/dev/ttys039",
		"/dev/ttys039": "/dev/ttys039",
		"??":           "",
		"":             "",
		"-":            "",
	}
	for in, want := range cases {
		if got := DevPath(in); got != want {
			t.Errorf("DevPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// A dead process must never be typed into: its pane is still open, with a
// shell back at a prompt, where a reply becomes a command.
func TestSendTextRefusesWhenTheAgentHasExited(t *testing.T) {
	// Pid 1 is alive but was certainly not started a moment ago, so the
	// start-time check is what has to catch this.
	tgt := TermTarget{Kind: TermITerm2, ID: "x", TTY: "/dev/ttys001",
		Pid: 1, Started: time.Now()}
	err := tgt.SendText([]byte("y\r"))
	if err == nil {
		t.Fatal("typed into a pane whose adopted process is gone")
	}
	if !IsTargetStale(err) {
		t.Errorf("error is not marked stale, so the target will never be re-adopted: %v", err)
	}

	// A pid that does not exist at all.
	tgt = TermTarget{Kind: TermITerm2, ID: "x", TTY: "/dev/ttys001", Pid: 0x7ffffffe}
	if err := tgt.SendText([]byte("y\r")); err == nil || !IsTargetStale(err) {
		t.Errorf("a vanished pid was not treated as stale: %v", err)
	}
}

func TestParsePlace(t *testing.T) {
	p, err := parsePlace("270 14 1\n")
	if err != nil || p != (itermPlace{window: "270", tab: 14, session: 1}) {
		t.Fatalf("got %+v, %v", p, err)
	}
	for _, bad := range []string{"", "270 14", "270 0 1", `270" & x 1 1`, "w 1 1 extra junk"} {
		if _, err := parsePlace(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	// A closed session reads as one, so its target is dropped as stale.
	if _, err := parsePlace("gone"); err == nil || !IsTargetStale(markStale(err)) {
		t.Errorf("gone: %v, want a stale target", err)
	}
}

// resetITerm2Gate gives a test the gate as a fresh daemon would have it.
func resetITerm2Gate(t *testing.T) {
	t.Helper()
	itermSlot = make(chan struct{}, 1)
	itermQuietUntil, itermBackoff = time.Time{}, 0
	// A slow call outlives its test, by design. Waited for at the end, so it
	// does not release a slot the next test has taken.
	slot := itermSlot
	t.Cleanup(func() {
		slot <- struct{}{}
		<-slot
	})
}

// A wedged iTerm2 keeps every Apple event it has been sent, so a slow call
// must hold the slot until it really ends rather than be killed and retried.
func TestITerm2CallHoldsTheSlotWhileASlowCallRuns(t *testing.T) {
	resetITerm2Gate(t)
	if _, err := runITerm2Call(exec.Command("sleep", "0.5"), "", 50*time.Millisecond); err == nil {
		t.Fatal("slow call reported success")
	}
	_, err := runITerm2Call(exec.Command("true"), "", 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "earlier request") {
		t.Fatalf("second call while the first runs: err = %v", err)
	}
}

func TestITerm2CallBacksOffAfterASlowCall(t *testing.T) {
	resetITerm2Gate(t)
	runITerm2Call(exec.Command("sleep", "0.2"), "", 50*time.Millisecond)
	time.Sleep(400 * time.Millisecond) // let the slow call end
	_, err := runITerm2Call(exec.Command("true"), "", 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "trying again") {
		t.Fatalf("call after a slow one: err = %v", err)
	}
	if itermBackoff != itermBackoffMin {
		t.Fatalf("backoff = %s, want %s", itermBackoff, itermBackoffMin)
	}
}

func TestITerm2CallBackoffClearsOnAPromptAnswer(t *testing.T) {
	resetITerm2Gate(t)
	itermBackoff = time.Minute
	if _, err := runITerm2Call(exec.Command("true"), "", time.Second); err != nil {
		t.Fatal(err)
	}
	if itermBackoff != 0 {
		t.Fatalf("backoff = %s after a prompt answer, want 0", itermBackoff)
	}
}

// Nothing reaches iTerm2 that macOS has not already allowed: a script sent
// before then raises the Automation prompt from a daemon nobody is watching.
func TestNoScriptWithoutPermission(t *testing.T) {
	closed := errors.New("not allowed")
	old := iTerm2Gate
	iTerm2Gate = func() error { return closed }
	defer func() { iTerm2Gate = old }()

	if _, err := ITerm2Sessions(); !errors.Is(err, closed) {
		t.Fatalf("ITerm2Sessions: got %v, want the gate's error", err)
	}
	if err := ITerm2SendText("w0t0p0:ABC", "/dev/ttys001", []byte("hi")); err == nil {
		t.Fatal("ITerm2SendText went ahead without permission")
	}
}

func TestAutomationErrorSaysWhatToDo(t *testing.T) {
	if automationError(AutomationAllowed) != nil {
		t.Fatal("allowed should not be an error")
	}
	for a, want := range map[Automation]string{
		AutomationNotAsked:   "muxscale up",
		AutomationDenied:     "System Settings",
		AutomationNotRunning: "not running",
	} {
		if err := automationError(a); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want it to mention %q", a, err, want)
		}
	}
}
