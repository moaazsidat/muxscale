package tsnode

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newACL(t *testing.T) *ACL {
	t.Helper()
	a, err := LoadACL(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// An unconfigured daemon must not be an open door. This is the single most
// important property in this file.
func TestEmptyACLPermitsNobody(t *testing.T) {
	a := newACL(t)
	for _, login := range []string{"alice@example.com", "", "anyone@anywhere"} {
		if a.Permits(Identity{LoginName: login}) {
			t.Errorf("empty ACL permitted %q; an unconfigured daemon must fail closed", login)
		}
	}
}

func TestOwnerAlwaysPermitted(t *testing.T) {
	a := newACL(t)
	if err := a.SetOwner("Alice@Example.com"); err != nil {
		t.Fatal(err)
	}
	// Identity comparison is case-insensitive: tailnet logins are email-shaped
	// and the case Tailscale reports is not guaranteed stable.
	if !a.Permits(Identity{LoginName: "alice@example.com"}) {
		t.Error("owner not permitted")
	}
	if !a.Permits(Identity{LoginName: "ALICE@EXAMPLE.COM"}) {
		t.Error("owner check should be case-insensitive")
	}
}

// A bad ACL edit must not be able to lock someone out of their own machine.
func TestOwnerCannotBeRemoved(t *testing.T) {
	a := newACL(t)
	_ = a.SetOwner("alice@example.com")
	err := a.Deny("alice@example.com")
	if err == nil {
		t.Fatal("removing the owner should be refused")
	}
	if !strings.Contains(err.Error(), "lock you out") {
		t.Errorf("the refusal should explain why: %v", err)
	}
	if !a.Permits(Identity{LoginName: "alice@example.com"}) {
		t.Error("owner lost access despite the refusal")
	}
}

func TestAllowAndDeny(t *testing.T) {
	a := newACL(t)
	_ = a.SetOwner("alice@example.com")

	if err := a.Allow("bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if !a.Permits(Identity{LoginName: "bob@example.com"}) {
		t.Error("bob should be permitted after Allow")
	}
	if a.Permits(Identity{LoginName: "carol@example.com"}) {
		t.Error("carol was never allowed")
	}

	if err := a.Deny("bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if a.Permits(Identity{LoginName: "bob@example.com"}) {
		t.Error("bob should be denied after Deny")
	}
}

func TestAllowIsIdempotent(t *testing.T) {
	a := newACL(t)
	for i := 0; i < 3; i++ {
		if err := a.Allow("bob@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(a.Allowed); n != 1 {
		t.Errorf("allowed list has %d entries after 3 identical Allows, want 1", n)
	}
}

func TestACLPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadACL(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = a.SetOwner("alice@example.com")
	_ = a.Allow("bob@example.com")

	b, err := LoadACL(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Permits(Identity{LoginName: "alice@example.com"}) {
		t.Error("owner did not survive reload")
	}
	if !b.Permits(Identity{LoginName: "bob@example.com"}) {
		t.Error("allowed identity did not survive reload")
	}
}

// Refusing to guess who is allowed is safer than proceeding with a partial or
// corrupted list.
func TestCorruptACLIsRefusedNotIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "acl.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadACL(dir); err == nil {
		t.Fatal("a corrupt ACL should be an error, not silently treated as empty")
	}
}

func TestACLFilePermissions(t *testing.T) {
	dir := t.TempDir()
	a, _ := LoadACL(dir)
	_ = a.SetOwner("alice@example.com")

	fi, err := os.Stat(filepath.Join(dir, "acl.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("acl.json mode = %o, want 600", perm)
	}
}

func TestMiddlewareBlocksAndExplains(t *testing.T) {
	a := newACL(t)
	_ = a.SetOwner("alice@example.com")

	var reached bool
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}), nil)

	// Permitted.
	req := httptest.NewRequest("GET", "/v1/sessions", nil)
	req = req.WithContext(contextWithIdentity(req.Context(),
		Identity{LoginName: "alice@example.com"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !reached || rec.Code != http.StatusOK {
		t.Errorf("permitted identity was blocked: %d", rec.Code)
	}

	// Denied, with actionable guidance.
	reached = false
	req = httptest.NewRequest("GET", "/v1/sessions", nil)
	req = req.WithContext(contextWithIdentity(req.Context(),
		Identity{LoginName: "mallory@evil.com"}))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if reached {
		t.Error("denied identity reached the handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "muxscale acl allow mallory@evil.com") {
		t.Errorf("denial should tell the user how to fix it, got: %s", rec.Body.String())
	}
}

// A request with no verified identity must be refused, not treated as
// anonymous: this endpoint can send input to a coding agent.
func TestMiddlewareRefusesUnverified(t *testing.T) {
	a := newACL(t)
	_ = a.SetOwner("alice@example.com")
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler reached without a verified identity")
	}), nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestDefaultHostnameIsAValidDNSLabel(t *testing.T) {
	got := DefaultHostname()
	if !strings.HasPrefix(got, "muxscale") {
		t.Errorf("hostname %q should start with muxscale", got)
	}
	if len(got) > 63 {
		t.Errorf("hostname %q exceeds the 63-char DNS label limit", got)
	}
	for _, r := range got {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !valid {
			t.Errorf("hostname %q contains %q, which is not valid in a DNS label", got, r)
		}
	}
	if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
		t.Errorf("hostname %q must not start or end with a hyphen", got)
	}
	t.Logf("default tailnet hostname: %s", got)
}

func TestOwnerLogin(t *testing.T) {
	a, err := LoadACL(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.OwnerLogin() != "" {
		t.Fatal("a fresh ACL has an owner")
	}
	if err := a.SetOwner(" Me@Example.com "); err != nil {
		t.Fatal(err)
	}
	if a.OwnerLogin() != "me@example.com" {
		t.Fatalf("owner = %q", a.OwnerLogin())
	}
}
