package tsnode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tailscale.com/client/local"
)

// NewSystem must say which of the two problems it hit, because they need
// different things done about them: a Tailscale that is not running is
// `tailscale up`, and one that is running but logged out is a login.
//
// Only meaningful where there is no system Tailscale to find. On a developer
// machine that has one there is no supported way to hide it -- the local client
// does not discover the daemon through anything a test can redirect on macOS --
// so this skips rather than asserting something the environment decides.
func TestNewSystemExplainsWhyItCannotStart(t *testing.T) {
	if _, err := NewSystem(context.Background(), SystemConfig{Port: 4443}); err == nil {
		t.Skip("a system Tailscale is running here; nothing to check")
	}

	_, err := NewSystem(context.Background(), SystemConfig{Port: 4443})
	if err == nil {
		t.Fatal("expected a refusal when the system Tailscale is unreachable")
	}
	// The message has to name Tailscale and suggest looking at it. "connection
	// refused" on its own sends people to the wrong place.
	if !strings.Contains(err.Error(), "Tailscale") {
		t.Errorf("error does not mention Tailscale: %v", err)
	}
}

// The identity gate is the security boundary and is shared between the tsnet
// listener and this one. A caller Tailscale cannot identify is refused, never
// treated as anonymous: this endpoint can type into a coding agent.
func TestWithIdentityRefusesAnUnidentifiableCaller(t *testing.T) {
	// A client pointed at a socket that is not there, so WhoIs cannot answer.
	t.Setenv("TS_SOCKET", t.TempDir()+"/definitely-not-a-socket")

	reached := false
	h := WithIdentity(&local.Client{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.RemoteAddr = "100.64.0.9:1234"
	h.ServeHTTP(rec, req)

	if reached {
		t.Fatal("the handler ran for a caller whose identity could not be verified")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// The ACL is the second gate and runs after identity. It fails closed: an
// unconfigured daemon must not be an open door to everyone on the tailnet.
func TestACLFailsClosedForAnUnknownIdentity(t *testing.T) {
	acl, err := LoadACL(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if acl.Permits(Identity{LoginName: "someone@example.com"}) {
		t.Fatal("an empty ACL permitted a stranger")
	}
	if acl.Permits(Identity{}) {
		t.Fatal("an empty ACL permitted a caller with no login name")
	}

	if err := acl.SetOwner("owner@example.com"); err != nil {
		t.Fatal(err)
	}
	if !acl.Permits(Identity{LoginName: "owner@example.com"}) {
		t.Error("the owner was refused")
	}
	// Case should not decide whether you get in.
	if !acl.Permits(Identity{LoginName: "Owner@Example.com"}) {
		t.Error("the owner was refused on a case difference")
	}
	if acl.Permits(Identity{LoginName: "someone@example.com"}) {
		t.Error("a stranger was permitted once an owner existed")
	}
}

// A denial says what to do about it. A bare 403 becomes a support ticket.
func TestACLDenialNamesTheFix(t *testing.T) {
	acl, err := LoadACL(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := acl.SetOwner("owner@example.com"); err != nil {
		t.Fatal(err)
	}

	var denied Identity
	h := acl.Middleware(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("the handler ran for a denied identity")
		}),
		func(id Identity, _ *http.Request) { denied = id },
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	stranger := Identity{LoginName: "stranger@example.com"}
	h.ServeHTTP(rec, req.WithContext(contextWithIdentity(req.Context(), stranger)))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if denied.LoginName != stranger.LoginName {
		t.Errorf("onDeny saw %q, want %q", denied.LoginName, stranger.LoginName)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "muxscale acl allow stranger@example.com") {
		t.Errorf("the refusal does not say how to fix it:\n%s", body)
	}
}

// The URL the daemon prints has to be the one that works. Printing https for a
// plain-HTTP listener -- or the reverse -- sends the user to a bare TLS
// handshake error with nothing to connect it to, which is exactly the failure
// this scheme selection exists to prevent.
func TestURLReportsTheSchemeActuallyServed(t *testing.T) {
	n := &SystemNode{port: 4443, dnsName: "studio.example.ts.net"}

	if got, want := n.URL(), "http://studio.example.ts.net:4443"; got != want {
		t.Errorf("without TLS: URL() = %q, want %q", got, want)
	}
	n.tls = true
	if got, want := n.URL(), "https://studio.example.ts.net:4443"; got != want {
		t.Errorf("with TLS: URL() = %q, want %q", got, want)
	}
}

// Listen decides the scheme; Serve must not run before it has. The two used to
// be one call, which meant the caller logged a guess at the scheme while a
// background goroutine was still deciding it -- wrong half the time, and a data
// race the rest.
func TestServeBeforeListenIsRefused(t *testing.T) {
	n := &SystemNode{port: 4443}
	err := n.Serve(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if err == nil {
		t.Fatal("Serve ran without a listener")
	}
	if !strings.Contains(err.Error(), "Listen") {
		t.Errorf("error does not name the missing step: %v", err)
	}
}
