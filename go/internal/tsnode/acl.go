package tsnode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// The daemon's own allowlist.
//
// Tailscale ACLs are defence in depth, not the only gate. A fresh tailnet
// allows a user's devices to reach each other on every port, and a user on a
// shared or company tailnet may not have admin rights to tighten it. Since this
// endpoint can send input to a coding agent, muxscale keeps an allowlist it
// controls, so an over-broad tailnet ACL cannot silently expose sessions.

// ACL is a persisted set of permitted tailnet identities.
type ACL struct {
	mu      sync.RWMutex
	path    string
	Allowed []string `json:"allowed"`
	// Owner is the identity that set the daemon up. It is always permitted and
	// cannot be removed, so a user cannot lock themselves out of their own
	// machine with a bad ACL edit.
	Owner string `json:"owner"`
}

// LoadACL reads the allowlist, creating an empty one if absent.
func LoadACL(stateDir string) (*ACL, error) {
	path := filepath.Join(stateDir, "acl.json")
	a := &ACL{path: path}

	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, a); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (refusing to guess who is allowed): %w", path, err)
	}
	a.path = path
	return a, nil
}

func (a *ACL) save() error {
	a.Allowed = normalise(a.Allowed)
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

// SetOwner records the setup identity, which is permanently permitted.
func (a *ACL) SetOwner(login string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Owner = strings.ToLower(strings.TrimSpace(login))
	return a.save()
}

// Allow permits an identity.
func (a *ACL) Allow(login string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return fmt.Errorf("empty identity")
	}
	for _, existing := range a.Allowed {
		if existing == login {
			return nil
		}
	}
	a.Allowed = append(a.Allowed, login)
	return a.save()
}

// Deny removes an identity. The owner cannot be removed.
func (a *ACL) Deny(login string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	login = strings.ToLower(strings.TrimSpace(login))
	if login == a.Owner {
		return fmt.Errorf(
			"%s set this daemon up and cannot be removed; "+
				"that would lock you out of your own machine", login)
	}
	out := a.Allowed[:0]
	for _, existing := range a.Allowed {
		if existing != login {
			out = append(out, existing)
		}
	}
	a.Allowed = out
	return a.save()
}

// Permits reports whether an identity may connect.
func (a *ACL) Permits(id Identity) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	login := strings.ToLower(id.LoginName)
	if login == "" {
		return false
	}
	if a.Owner != "" && login == a.Owner {
		return true
	}
	// An empty allowlist with no owner yet means the daemon has not been set up.
	// Fail closed: an unconfigured daemon must not be an open door.
	for _, allowed := range a.Allowed {
		if allowed == login {
			return true
		}
	}
	return false
}

// List returns the permitted identities, owner first.
func (a *ACL) List() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, 0, len(a.Allowed)+1)
	if a.Owner != "" {
		out = append(out, a.Owner+" (owner)")
	}
	for _, l := range a.Allowed {
		if l != a.Owner {
			out = append(out, l)
		}
	}
	return out
}

// Middleware refuses requests from identities the ACL does not permit.
//
// Runs after withIdentity, so the identity is already verified by Tailscale;
// this is the second, independent gate.
func (a *ACL) Middleware(next http.Handler, onDeny func(Identity, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := IdentityFrom(r)
		if !ok {
			http.Error(w, "no verified identity", http.StatusForbidden)
			return
		}
		if !a.Permits(id) {
			if onDeny != nil {
				onDeny(id, r)
			}
			// Say what to do about it. A bare 403 turns into a support ticket.
			http.Error(w, fmt.Sprintf(
				"%s is not permitted to reach this muxscale daemon.\n"+
					"On the host machine, run: muxscale acl allow %s",
				id.LoginName, id.LoginName), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func normalise(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
