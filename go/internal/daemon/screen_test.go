package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/attach"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// agentSession registers a session standing in for a discovered agent, so the
// shell endpoint has a working directory to inherit.
func agentSession(t *testing.T, reg *Registry, cwd string) *protocol.Session {
	t.Helper()
	return reg.Upsert("claude-code", "sess-"+filepath.Base(cwd), func(s *protocol.Session) {
		s.Cwd = cwd
		s.Attachment = protocol.AttachTranscript
		s.Fidelity = protocol.FidelityStructured
	})
}

func waitForShellExit(t *testing.T, m *Manager, id string) {
	t.Helper()
	_ = m.Kill(id)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := m.reg.Get(id); ok && s.State.Terminal() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunShellInheritsTheAgentsWorkingDirectory(t *testing.T) {
	m, reg, _ := testManager(t)
	dir := t.TempDir()
	parent := agentSession(t, reg, dir)

	sh, err := m.RunShell(parent.ID, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { waitForShellExit(t, m, sh.ID) })

	if sh.Cwd != dir {
		t.Errorf("shell cwd = %q, want the agent's %q", sh.Cwd, dir)
	}
	if sh.ParentID != parent.ID {
		t.Errorf("ParentID = %q, want %q -- without it the app lists the shell as a peer session", sh.ParentID, parent.ID)
	}
	if sh.AgentID != ShellAgentID {
		t.Errorf("AgentID = %q, want %q", sh.AgentID, ShellAgentID)
	}
	if !sh.AllowRemoteResize {
		t.Error("a client-created shell must permit remote resize; it is the only viewer")
	}
}

// The caller names a session, never a directory. A cwd parameter would turn one
// authenticated endpoint into "open a shell anywhere on this machine".
func TestShellEndpointTakesNoDirectoryFromTheCaller(t *testing.T) {
	m, reg, _ := testManager(t)
	d := &Daemon{mgr: m, reg: reg}
	mux := http.NewServeMux()
	d.screenRoutes(mux)

	dir := t.TempDir()
	parent := agentSession(t, reg, dir)

	body := `{"session_id":"` + parent.ID + `","cols":80,"rows":24,"dir":"/etc","cwd":"/etc"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/shell", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got protocol.Session
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { waitForShellExit(t, m, got.ID) })

	if got.Cwd != dir {
		t.Errorf("cwd = %q, want %q -- the request's dir/cwd fields must be ignored", got.Cwd, dir)
	}
}

func TestRunShellIsIdempotentWhileTheShellLives(t *testing.T) {
	m, reg, _ := testManager(t)
	parent := agentSession(t, reg, t.TempDir())

	first, err := m.RunShell(parent.ID, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { waitForShellExit(t, m, first.ID) })

	second, err := m.RunShell(parent.ID, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("second call opened shell %s, want the existing %s -- "+
			"reopening the tab must not strand a process or lose scrollback",
			second.ID, first.ID)
	}
}

func TestRunShellRejectsASessionWithNoKnownDirectory(t *testing.T) {
	m, reg, _ := testManager(t)
	s := reg.Upsert("codex", "no-cwd", nil)

	if _, err := m.RunShell(s.ID, 80, 24); err == nil {
		t.Fatal("expected a refusal: guessing a directory would open a shell somewhere arbitrary")
	}
	if _, err := m.RunShell("does-not-exist", 80, 24); err == nil {
		t.Fatal("expected a refusal for an unknown session")
	}
}

// Spec §5.5: a viewer must not reflow a PTY that other viewers share.
func TestResizeRefusedOnSessionsWithOtherViewers(t *testing.T) {
	m, reg, _ := testManager(t)
	d := &Daemon{mgr: m, reg: reg}
	mux := http.NewServeMux()
	d.screenRoutes(mux)

	dir := t.TempDir()
	parent := agentSession(t, reg, dir)

	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/resize",
		strings.NewReader(`{"session_id":"`+parent.ID+`","cols":52,"rows":20}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: an agent session the human is also watching must not reflow", rec.Code)
	}

	// The shell the client opened for itself is a different matter.
	sh, err := m.RunShell(parent.ID, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { waitForShellExit(t, m, sh.ID) })

	req = httptest.NewRequest(http.MethodPost, "/v1/sessions/resize",
		strings.NewReader(`{"session_id":"`+sh.ID+`","cols":52,"rows":20}`))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d on a client-owned shell: %s", rec.Code, rec.Body.String())
	}
}

func TestResizeRejectsZeroDimensions(t *testing.T) {
	m, reg, _ := testManager(t)
	parent := agentSession(t, reg, t.TempDir())
	sh, err := m.RunShell(parent.ID, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { waitForShellExit(t, m, sh.ID) })

	// A phone reports 0 rows for a moment while the keyboard animates. Passing
	// that through would leave the PTY at 0 and the agent drawing into nothing.
	if err := m.Resize(sh.ID, 52, 0); err == nil {
		t.Fatal("expected a refusal for a zero dimension")
	}
}

func TestScreenStreamOpensWithASnapshotThenSendsDeltas(t *testing.T) {
	m, reg, _ := testManager(t)
	d := &Daemon{mgr: m, reg: reg}
	mux := http.NewServeMux()
	d.screenRoutes(mux)

	parent := agentSession(t, reg, t.TempDir())
	sh, err := m.RunShell(parent.ID, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { waitForShellExit(t, m, sh.ID) })

	srv := httptest.NewServer(mux)

	// The stream is open-ended, so it has to be cancelled explicitly before the
	// server is closed -- httptest.Server.Close waits on in-flight handlers, and
	// this one is designed never to return on its own.
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); srv.Close() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/v1/sessions/screen/stream?session="+sh.ID+"&fps=20", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}

	events := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			if name, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				select {
				case events <- name:
				default:
				}
			}
		}
	}()

	if got := nextEvent(t, events); got != "snapshot" {
		t.Fatalf("first event = %q, want snapshot: a client must never have to "+
			"render from a delta with no baseline", got)
	}

	// Make the screen change.
	if err := m.SendInput(sh.ID, "echo muxscale-stream-probe", "", true, attach.Fence{}, "test"); err != nil {
		t.Fatal(err)
	}
	if got := nextEvent(t, events); got != "delta" {
		t.Fatalf("event after output = %q, want delta", got)
	}
}

func TestScreenStream404sForASessionWithNoScreen(t *testing.T) {
	m, reg, _ := testManager(t)
	d := &Daemon{mgr: m, reg: reg}
	mux := http.NewServeMux()
	d.screenRoutes(mux)

	parent := agentSession(t, reg, t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/screen/stream?session="+parent.ID, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// 404 before the headers go out, so the client sees a failed request rather
	// than an established stream that never produces a frame.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}

func TestClampFPS(t *testing.T) {
	cases := map[string]int{
		"":         defaultFPS,
		"nonsense": defaultFPS,
		"12":       12,
		"1":        minFPS,
		"-5":       minFPS,
		"1000":     maxFPS,
	}
	for in, want := range cases {
		if got := clampFPS(in); got != want {
			t.Errorf("clampFPS(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestLoginShellPrefersTheUsersChoice(t *testing.T) {
	t.Setenv("SHELL", "/usr/bin/fish")
	if got := loginShell(); got != "/usr/bin/fish" {
		t.Errorf("loginShell() = %q, want the user's $SHELL", got)
	}
	t.Setenv("SHELL", "")
	if got := loginShell(); got != "/bin/sh" {
		t.Errorf("loginShell() with no $SHELL = %q, want /bin/sh", got)
	}
}

func nextEvent(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a stream event")
		return ""
	}
}

// A shell exits by signal when it is hung up, and ExitCode reports that as a
// negative number. Without recording that the close was asked for, every
// terminal tab the user shut themselves came back badged CRASHED.
func TestAClosedShellReportsExitedNotCrashed(t *testing.T) {
	m, reg, _ := testManager(t)
	parent := agentSession(t, reg, t.TempDir())
	sh, err := m.RunShell(parent.ID, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	waitForShellExit(t, m, sh.ID)

	got, ok := reg.Get(sh.ID)
	if !ok {
		t.Fatal("session vanished")
	}
	if got.State != protocol.StateExited {
		t.Errorf("state = %s, want %s", got.State, protocol.StateExited)
	}
	if got.Guess.Reason != "closed" {
		t.Errorf("reason = %q, want %q", got.Guess.Reason, "closed")
	}
}
