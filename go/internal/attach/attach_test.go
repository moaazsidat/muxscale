package attach

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// ---- input encoding ----

func TestEncodeTextSingleLine(t *testing.T) {
	got := string(EncodeText("continue", true))
	if got != "continue\r" {
		t.Errorf("got %q, want %q", got, "continue\r")
	}
	if got := string(EncodeText("continue", false)); got != "continue" {
		t.Errorf("no-submit got %q", got)
	}
}

// Multi-line text must arrive as one prompt. Without bracketed paste a
// three-line reply fires as three separate prompts, which is both wrong and
// alarming to watch happen remotely.
func TestEncodeTextMultilineUsesBracketedPaste(t *testing.T) {
	got := string(EncodeText("line one\nline two", true))
	if !strings.HasPrefix(got, pasteStart) {
		t.Error("multiline text must start with the bracketed-paste marker")
	}
	if !strings.Contains(got, pasteEnd) {
		t.Error("multiline text must close the bracketed-paste marker")
	}
	if !strings.HasSuffix(got, "\r") {
		t.Error("submit must still append a carriage return")
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(got, pasteStart), pasteEnd+"\r")
	if inner != "line one\nline two" {
		t.Errorf("paste body = %q", inner)
	}
}

func TestEncodeTextNormalisesLineEndings(t *testing.T) {
	got := string(EncodeText("a\r\nb\rc", false))
	body := strings.TrimSuffix(strings.TrimPrefix(got, pasteStart), pasteEnd)
	if body != "a\nb\nc" {
		t.Errorf("got %q, want CRLF and CR normalised to LF", body)
	}
}

func TestEncodeKey(t *testing.T) {
	cases := map[NamedKey]string{
		KeyEnter: "\r", KeyEscape: "\x1b", KeyCtrlC: "\x03", KeyUp: "\x1b[A",
	}
	for k, want := range cases {
		got, err := EncodeKey(k)
		if err != nil {
			t.Fatalf("EncodeKey(%s): %v", k, err)
		}
		if string(got) != want {
			t.Errorf("EncodeKey(%s) = %q, want %q", k, got, want)
		}
	}
	if _, err := EncodeKey("not-a-key"); err == nil {
		t.Error("expected an error for an unknown key")
	}
}

// ---- fencing ----

func TestFenceBlocksStaleInput(t *testing.T) {
	f := Fence{ExpectPromptID: "p1"}
	if err := f.Check("p1", "WAITING_INPUT"); err != nil {
		t.Errorf("matching fence should pass: %v", err)
	}
	err := f.Check("p2", "WAITING_INPUT")
	if err == nil {
		t.Fatal("a reply fenced to p1 must not be delivered while the session is on p2")
	}
	var fe *FenceError
	if !asFenceError(err, &fe) || fe.Field != "prompt_id" {
		t.Errorf("expected a prompt_id FenceError, got %v", err)
	}
}

func TestFenceChecksState(t *testing.T) {
	f := Fence{ExpectState: "WAITING_APPROVAL"}
	if err := f.Check("", "WAITING_APPROVAL"); err != nil {
		t.Errorf("matching state should pass: %v", err)
	}
	if err := f.Check("", "RUNNING"); err == nil {
		t.Error("state fence should block when the session has moved on")
	}
}

func TestEmptyFenceAllowsAnything(t *testing.T) {
	var f Fence
	if !f.IsEmpty() {
		t.Error("zero Fence should report empty")
	}
	if err := f.Check("anything", "anything"); err != nil {
		t.Errorf("empty fence should not block: %v", err)
	}
}

func asFenceError(err error, out **FenceError) bool {
	fe, ok := err.(*FenceError)
	if ok {
		*out = fe
	}
	return ok
}

// ---- pty wrap ----

func TestWrapCapturesOutputAndExitCode(t *testing.T) {
	w, err := Wrap(WrapOptions{Command: []string{"echo", "hello from the pty"}})
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, w)

	select {
	case <-w.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit")
	}
	if !strings.Contains(buf.String(), "hello from the pty") {
		t.Errorf("output = %q", buf.String())
	}
	if code := w.ExitCode(); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestWrapExitCodePropagates(t *testing.T) {
	w, err := Wrap(WrapOptions{Command: []string{"sh", "-c", "exit 3"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, w)
	<-w.Exited()
	if code := w.ExitCode(); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

// The point of owning the PTY: we can write to the agent's stdin.
func TestWrapWriteReachesStdin(t *testing.T) {
	w, err := Wrap(WrapOptions{Command: []string{"sh", "-c", "read line; echo GOT:$line"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(EncodeText("ping", true)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, w); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading output")
	}
	if !strings.Contains(buf.String(), "GOT:ping") {
		t.Errorf("stdin did not reach the process; output = %q", buf.String())
	}
}

func TestWrapSetsEnvMarker(t *testing.T) {
	w, err := Wrap(WrapOptions{Command: []string{"sh", "-c", "echo marker=$MUXSCALE_WRAPPED"}})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, w)
	<-w.Exited()
	if !strings.Contains(buf.String(), "marker=1") {
		t.Errorf("MUXSCALE_WRAPPED not set in the child env; got %q", buf.String())
	}
}

func TestWrapRunsInGivenDir(t *testing.T) {
	dir := t.TempDir()
	w, err := Wrap(WrapOptions{Command: []string{"pwd"}, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, w)
	<-w.Exited()
	// macOS reports /private/var for /var, so compare the resolved paths.
	want, _ := os.Readlink(dir)
	if want == "" {
		want = dir
	}
	got := strings.TrimSpace(buf.String())
	if !strings.HasSuffix(got, strings.TrimPrefix(dir, "/private")) && got != want {
		t.Errorf("pwd = %q, want %q", got, dir)
	}
}

// A hung session that cannot be killed is a support ticket, so Close must
// escalate rather than wait forever.
func TestWrapCloseKillsIgnoredTerm(t *testing.T) {
	w, err := Wrap(WrapOptions{Command: []string{"sh", "-c", "trap '' TERM; sleep 30"}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, w) }()
	start := time.Now()
	if err := w.Close(200 * time.Millisecond); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Close took %v; escalation to SIGKILL is too slow", elapsed)
	}
}

func TestWrapWriteAfterExitFails(t *testing.T) {
	w, err := Wrap(WrapOptions{Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, w)
	<-w.Exited()
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("writing to an exited session should fail, not silently succeed")
	}
}

func TestWrapResize(t *testing.T) {
	w, err := Wrap(WrapOptions{Command: []string{"sleep", "5"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(time.Second)
	if err := w.Resize(120, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if w.Cols != 120 || w.Rows != 40 {
		t.Errorf("size = %dx%d, want 120x40", w.Cols, w.Rows)
	}
}

// ---- discovery ----

func TestDiscoverRunsOnThisPlatform(t *testing.T) {
	ps, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// Zero agents is a legitimate result; what matters is that parsing works
	// and nothing is malformed.
	for _, p := range ps {
		if p.Pid <= 0 {
			t.Errorf("bad pid in %+v", p)
		}
		if _, known := AgentBinaries[p.Command]; !known {
			t.Errorf("discovered %q which is not a known agent binary", p.Command)
		}
		t.Logf("found %s pid=%d cwd=%s ancestor=%s writable_via=%q",
			p.Command, p.Pid, p.Cwd, p.Ancestor, p.WritableVia())
	}
	t.Logf("discovered %d agent process(es)", len(ps))
}

func TestWritableViaMapsAncestors(t *testing.T) {
	cases := map[string]string{
		"tmux": "tmux", "zellij": "zellij", "kitty": "termctl:kitty",
		"wezterm": "termctl:wezterm", "ghostty": "", "": "",
	}
	for ancestor, want := range cases {
		p := Process{Ancestor: ancestor}
		if got := p.WritableVia(); got != want {
			t.Errorf("ancestor %q -> %q, want %q", ancestor, got, want)
		}
	}
}
