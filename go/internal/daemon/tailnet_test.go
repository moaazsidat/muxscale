package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/muxscale/muxscale/go/internal/attach"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// tailnetDaemon builds a daemon that looks like a running one, with the
// listener replaced by a stub so the lifecycle can be driven without a real
// Tailscale.
func tailnetDaemon(t *testing.T, start func(context.Context, string, http.Handler, bool) (string, error)) *Daemon {
	t.Helper()
	m, reg, st := testManager(t)
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Daemon{
		cfg:            Config{},
		mgr:            m,
		reg:            reg,
		store:          st,
		log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		mux:            http.NewServeMux(),
		stateDir:       t.TempDir(),
		runCtx:         runCtx,
		tailnetStarter: start,
	}
}

// The listener used to be start-only, so turning it on meant restarting the
// daemon -- which kills every PTY it owns. This is the lifecycle that makes
// "start the daemon now, enable the tailnet later" safe: on, off, and on again
// without disturbing anything else the daemon is doing.
func TestTailnetListenerCanBeTurnedOnAndOffAtRuntime(t *testing.T) {
	var starts, cancels atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := tailnetDaemon(t, func(c context.Context, _ string, _ http.Handler, useSystem bool) (string, error) {
		starts.Add(1)
		if useSystem {
			return "https://studio.example.ts.net:4443", nil
		}
		// Stands in for the serve goroutine: the cancel is what a real
		// listener would observe to stop serving.
		go func() { <-c.Done(); cancels.Add(1) }()
		return "", nil
	})

	if _, _, on := d.TailnetEnabled(); on {
		t.Fatal("the listener reports itself up before it was enabled")
	}
	if err := d.DisableTailnet(); err == nil {
		t.Error("stopping a listener that is not running should fail, not silently succeed")
	}

	url, err := d.EnableTailnet(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://studio.example.ts.net:4443" {
		t.Errorf("url = %q, want the system listener's URL", url)
	}
	mode, gotURL, on := d.TailnetEnabled()
	if !on || mode != "system" || gotURL != url {
		t.Fatalf("TailnetEnabled = (%q, %q, %v), want (system, %q, true)", mode, gotURL, on, url)
	}

	// Enabling twice must be refused rather than starting a second listener on
	// the same port.
	if _, err := d.EnableTailnet(ctx, true); err == nil {
		t.Error("enabling an already-running listener should fail")
	}
	if starts.Load() != 1 {
		t.Errorf("starts = %d, want 1 -- a second listener was started", starts.Load())
	}

	if err := d.DisableTailnet(); err != nil {
		t.Fatal(err)
	}
	if mode, url, on := d.TailnetEnabled(); on || mode != "" || url != "" {
		t.Fatalf("after stopping: (%q, %q, %v), want cleared", mode, url, on)
	}

	// Off then on again: the second listener is a fresh one, and the first
	// listener's context really was cancelled.
	if _, err := d.EnableTailnet(ctx, false); err != nil {
		t.Fatal(err)
	}
	if mode, _, on := d.TailnetEnabled(); !on || mode != "tsnet" {
		t.Fatalf("mode = %q, want tsnet", mode)
	}
	if err := d.DisableTailnet(); err != nil {
		t.Fatal(err)
	}
	if cancels.Load() != 1 {
		t.Errorf("cancels = %d, want 1 -- the first listener was never told to stop", cancels.Load())
	}
}

// A listener enabled by an HTTP request must not die with that request.
//
// The first version parented the listener's context to the request context, so
// `muxscale tailnet on` bound the port, printed a working URL, and the listener
// was torn down the instant the handler returned -- a green light over a dead
// port. Only caught by connecting to it.
func TestListenerOutlivesTheRequestThatEnabledIt(t *testing.T) {
	var listenerCtx context.Context
	d := tailnetDaemon(t, func(c context.Context, _ string, _ http.Handler, _ bool) (string, error) {
		listenerCtx = c
		return "https://studio.example.ts.net:4443", nil
	})

	reqCtx, cancelReq := context.WithCancel(context.Background())
	if _, err := d.EnableTailnet(reqCtx, true); err != nil {
		t.Fatal(err)
	}
	cancelReq() // the request is over

	if err := listenerCtx.Err(); err != nil {
		t.Fatalf("the listener's context was cancelled with the request that enabled it: %v", err)
	}
	if _, _, on := d.TailnetEnabled(); !on {
		t.Fatal("the listener stopped when the request that enabled it finished")
	}

	// It stops when the daemon does.
	if err := d.DisableTailnet(); err != nil {
		t.Fatal(err)
	}
	if listenerCtx.Err() == nil {
		t.Error("stopping should cancel the listener's context")
	}
}

// A listener that fails to come up must leave the daemon in the off state, not
// half-on: a status request after a failed start should say "off", and a
// second attempt should be allowed.
func TestAFailedTailnetStartLeavesItOff(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("tailscale is not logged in")
	d := tailnetDaemon(t, func(context.Context, string, http.Handler, bool) (string, error) {
		return "", boom
	})

	if _, err := d.EnableTailnet(ctx, true); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the starter's error", err)
	}
	if _, _, on := d.TailnetEnabled(); on {
		t.Fatal("a failed start left the listener reported as up")
	}
	if err := d.DisableTailnet(); err == nil {
		t.Error("after a failed start, stopping should report that nothing is running")
	}
}

// Before Run has wired the handler there is nothing to serve, and saying so is
// better than starting a listener with a nil handler.
func TestEnableTailnetRefusesBeforeTheDaemonIsRunning(t *testing.T) {
	d := tailnetDaemon(t, func(context.Context, string, http.Handler, bool) (string, error) {
		t.Fatal("the starter must not be called")
		return "", nil
	})
	d.mux = nil

	if _, err := d.EnableTailnet(context.Background(), true); err == nil {
		t.Fatal("enabling the tailnet before Run should fail")
	}
}

// GET /v1/tailnet answers with the off state rather than an error: a client
// asking whether the tailnet is up has not made a mistake.
func TestTailnetRouteReportsTheOffState(t *testing.T) {
	_, mux := authDaemon(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/tailnet", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"enabled":false`) {
		t.Errorf("body = %s, want enabled:false", body)
	}
}

// A termctl-adopted session is writable even though muxscale owns no PTY for
// it -- that is the whole point of adopting one -- and the report the CLI
// prints must say so, or the user is told to restart a session that is already
// answering.
func TestAdoptRowsReportWritabilityByChannel(t *testing.T) {
	m, reg, _ := testManager(t)

	adopted := reg.Upsert("claude-code", "pid:4242", func(s *protocol.Session) {
		s.Cwd = "/home/dev/app"
		s.Attachment = protocol.AttachTermctl
		s.Fidelity = protocol.FidelityStructured
	})
	readOnly := reg.Upsert("claude-code", "pid:4343", func(s *protocol.Session) {
		s.Attachment = protocol.AttachTranscript
		s.Fidelity = protocol.FidelityReadOnly
	})
	gone := reg.Upsert("claude-code", "pid:4444", func(s *protocol.Session) {})
	reg.SetState(gone.ID, protocol.StateGuess{State: protocol.StateExited, Confidence: 1})

	m.mu.Lock()
	m.termctlTargets[adopted.ID] = attach.TermTarget{Kind: attach.TermITerm2, ID: "w0t0p0"}
	m.mu.Unlock()

	rows := map[string]AdoptRow{}
	for _, r := range m.adoptRows() {
		rows[r.ID] = r
	}
	if r := rows[adopted.ID]; !r.Writable || r.Attachment != protocol.AttachTermctl {
		t.Errorf("adopted session = %+v, want writable with termctl", r)
	}
	if r := rows[readOnly.ID]; r.Writable {
		t.Errorf("transcript-only session = %+v, want read-only", r)
	}
	if _, ok := rows[gone.ID]; ok {
		t.Error("an exited session should not be in the adoption report")
	}
}

// `muxscale connect` used to last only until the daemon restarted, so the
// phone lost its machine every time the service came back up.
func TestTailnetSettingIsRememberedThroughTheRoute(t *testing.T) {
	d := tailnetDaemon(t, func(context.Context, string, http.Handler, bool) (string, error) {
		return "https://studio.example.ts.net:4443", nil
	})
	d.cfg.LocalToken = testToken
	mux := http.NewServeMux()
	d.mux = mux
	d.routes(mux)
	post := func(body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/tailnet", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}

	post(`{"enable":true,"system":true}`)
	if got, _ := d.store.Setting(settingTailnet); got != "system" {
		t.Fatalf("after on: setting = %q, want system", got)
	}
	post(`{"enable":false}`)
	if got, _ := d.store.Setting(settingTailnet); got != "off" {
		t.Fatalf("after off: setting = %q, want off", got)
	}
}
