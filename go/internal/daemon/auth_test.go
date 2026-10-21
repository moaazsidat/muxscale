package daemon

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/muxscale/muxscale/go/internal/tsnode"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef" // notasecret

// testDaemon builds a Daemon wired the way Run would, for tests that exercise
// the HTTP surface. Constructing the struct literal by hand is what tests used
// to do, and it left d.log nil -- which was fine until something on a refusal
// path logged, and then a 401 became a segfault.
func testDaemon(t *testing.T, token string) (*Daemon, *http.ServeMux) {
	t.Helper()
	m, reg, st := testManager(t)
	d := &Daemon{
		cfg:   Config{LocalToken: token},
		mgr:   m,
		reg:   reg,
		store: st,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	mux := http.NewServeMux()
	d.routes(mux)
	return d, mux
}

func authDaemon(t *testing.T) (*Daemon, *http.ServeMux) {
	t.Helper()
	return testDaemon(t, testToken)
}

// Every control-API route, not a sample of them. A route added later without
// going through d.handle is exactly the failure this is here to catch, and a
// spot check would miss it.
func controlRoutes() []struct {
	method, path string
	body         string
} {
	return []struct {
		method, path string
		body         string
	}{
		{http.MethodGet, "/v1/sessions", ""},
		{http.MethodGet, "/v1/approvals", ""},
		{http.MethodPost, "/v1/approvals/decide", `{"id":"x","behavior":"allow"}`},
		{http.MethodPost, "/v1/sessions/run", `{"command":["true"]}`},
		{http.MethodPost, "/v1/sessions/input", `{"session_id":"x","text":"y"}`},
		{http.MethodPost, "/v1/sessions/type", `{"session_id":"x","data":"y"}`},
		{http.MethodPost, "/v1/sessions/kill", `{"session_id":"x"}`},
		{http.MethodPost, "/v1/sessions/interrupt", `{"session_id":"x"}`},
		{http.MethodPost, "/v1/sessions/rename", `{"session_id":"x","name":"y"}`},
		{http.MethodPost, "/v1/sessions/upload?session=x&name=a.png", "bytes"},
		{http.MethodGet, "/v1/sessions/transcript?session=x", ""},
		{http.MethodGet, "/v1/sessions/screen?session=x", ""},
		{http.MethodGet, "/v1/status", ""},
		{http.MethodGet, "/v1/tailnet", ""},
		{http.MethodPost, "/v1/tailnet", `{"enable":false}`},
		{http.MethodPost, "/v1/adopt", ""},
		{http.MethodGet, "/v1/acl", ""},
		{http.MethodGet, "/v1/audit", ""},
		{http.MethodPost, "/v1/sessions/shell", `{"session_id":"x"}`},
		{http.MethodPost, "/v1/sessions/resize", `{"session_id":"x","cols":80,"rows":24}`},
		{http.MethodGet, "/v1/sessions/screen/stream?session=x", ""},
	}
}

// Before this, anything that could reach loopback could approve a pending tool
// call, type into a running agent, or open a shell in one of its project
// directories. Loopback is not a same-user boundary: other local users reach
// it, so do sandboxed processes, and a browser can be walked onto it by DNS
// rebinding.
func TestControlAPIRefusesAnUnauthenticatedCaller(t *testing.T) {
	_, mux := authDaemon(t)

	for _, tc := range controlRoutes() {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 -- this route is reachable without a token", rec.Code)
			}
		})
	}
}

func TestControlAPIAcceptsTheLocalToken(t *testing.T) {
	_, mux := authDaemon(t)

	for _, tc := range controlRoutes() {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+testToken)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			// Anything but 401 means auth let it through; the handler is free to
			// reject the made-up session id on its own terms.
			if rec.Code == http.StatusUnauthorized {
				t.Fatalf("the correct token was refused")
			}
		})
	}
}

func TestControlAPIRefusesAWrongToken(t *testing.T) {
	_, mux := authDaemon(t)

	for _, header := range []string{
		"Bearer wrong",
		"Bearer ",
		"",
		"Basic " + testToken,
		// A prefix of the real token: a length-only or prefix comparison would
		// let this through.
		"Bearer " + testToken[:20],
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q got %d, want 401", header, rec.Code)
		}
	}
}

// The phone has no token and must never need one: it is authenticated by
// Tailscale identity, which the tailnet listener has already verified and
// checked against the ACL before the request reaches the mux.
func TestATailnetIdentityIsAcceptedWithoutAToken(t *testing.T) {
	_, mux := authDaemon(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req = req.WithContext(tsnode.ContextWithIdentity(req.Context(),
		tsnode.Identity{LoginName: "owner@example.com"}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a verified tailnet caller was refused", rec.Code)
	}
}

// A blank token must deny, not admit. The hook endpoint takes the opposite
// view deliberately -- a hook that cannot authenticate has to fail open so the
// agent is not blocked -- but nothing here has that obligation.
func TestABlankConfiguredTokenDeniesRatherThanAdmits(t *testing.T) {
	_, mux := testDaemon(t, "")

	for _, header := range []string{"", "Bearer ", "Bearer anything"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q got %d with no token configured, want 401", header, rec.Code)
		}
	}
}

// The refusal has to say how to fix it. A bare 401 from a daemon on your own
// machine is a puzzle, not an error.
func TestTheRefusalSaysWhatToDo(t *testing.T) {
	_, mux := authDaemon(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Authorization: Bearer") {
		t.Errorf("the refusal does not say how to authenticate:\n%s", body)
	}
}
