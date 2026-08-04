// Package tsnode exposes the daemon on a Tailscale tailnet as its own node.
//
// The daemon embeds Tailscale via tsnet rather than requiring the system
// tailscaled. Three consequences, all deliberate:
//
//   - No sudo, no kernel networking, no interference with an existing Tailscale
//     setup on the machine.
//   - The daemon appears as its own tailnet node ("muxscale-<host>"), so ACLs
//     can grant a phone access to muxscale specifically without granting it SSH
//     to the whole workstation.
//   - Nothing to install. This is what keeps "one curl command" honest.
package tsnode

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

// Config configures the tailnet node.
type Config struct {
	// Hostname is the node name on the tailnet. Defaults to muxscale-<host>.
	Hostname string
	// StateDir holds the node key. Must be 0700: it is a credential.
	StateDir string
	// AuthKey is an optional reusable auth key. When empty, tsnet prints an
	// interactive login URL on first run.
	AuthKey string
	// Port is the HTTPS listen port on the tailnet.
	Port int
	// Logf receives tsnet's chatty logs; nil discards them.
	Logf func(format string, args ...any)
}

// Node is a running tailnet node.
type Node struct {
	srv  *tsnet.Server
	lc   *local.Client
	ln   net.Listener
	cfg  Config
	once sync.Once

	// authURL is captured from tsnet's user-facing log because srv.Up() blocks
	// until login completes -- so without this, `muxscale status` would show a
	// blank state during exactly the window when the user needs the URL.
	mu      sync.RWMutex
	authURL string
}

// AuthURL returns the interactive login URL, if one is pending.
func (n *Node) AuthURL() string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.authURL
}

func (n *Node) setAuthURL(u string) {
	n.mu.Lock()
	n.authURL = u
	n.mu.Unlock()
}

// loginURLRe matches the URL tsnet prints when interactive login is required.
var loginURLRe = regexp.MustCompile(`https://login\.tailscale\.com/\S+`)

// DefaultHostname derives the tailnet node name from the machine's hostname.
// Tailscale requires a DNS label, so this sanitises rather than trusting it.
func DefaultHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "muxscale"
	}
	// Strip the domain and any characters that are not valid in a DNS label.
	h = strings.SplitN(h, ".", 2)[0]
	h = strings.ToLower(h)
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ':
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "muxscale"
	}
	if len(name) > 50 { // leave room for the prefix within the 63-char label limit
		name = name[:50]
	}
	return "muxscale-" + name
}

// New prepares a tailnet node. It does not connect until Start.
func New(cfg Config) (*Node, error) {
	if cfg.Hostname == "" {
		cfg.Hostname = DefaultHostname()
	}
	if cfg.Port == 0 {
		cfg.Port = 4443
	}
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("tsnode: StateDir is required")
	}
	// The node key lives here. Anything looser than 0700 would let another
	// local user impersonate this machine on the tailnet.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}

	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	n := &Node{cfg: cfg}
	n.srv = &tsnet.Server{
		Hostname: cfg.Hostname,
		Dir:      filepath.Join(cfg.StateDir, "node"),
		AuthKey:  cfg.AuthKey,
		Logf:     logf,
		UserLogf: func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			if u := loginURLRe.FindString(msg); u != "" {
				n.setAuthURL(u)
			}
			log.Print(msg) // login URLs must reach the user
		},
	}
	return n, nil
}

// Start brings the node up and begins listening.
//
// handler is wrapped so every request carries the caller's verified tailnet
// identity; see Identity.
func (n *Node) Start(ctx context.Context, handler http.Handler) error {
	if _, err := n.srv.Up(ctx); err != nil {
		return fmt.Errorf("bringing the tailnet node up: %w", err)
	}
	lc, err := n.srv.LocalClient()
	if err != nil {
		return fmt.Errorf("tailscale local client: %w", err)
	}
	n.lc = lc

	// ListenTLS obtains a cert for <hostname>.<tailnet>.ts.net from Tailscale.
	// It requires HTTPS certificates to be enabled for the tailnet, so the
	// error path here needs to say that rather than surfacing a raw TLS error.
	ln, err := n.srv.ListenTLS("tcp", fmt.Sprintf(":%d", n.cfg.Port))
	if err != nil {
		return fmt.Errorf(
			"listening on the tailnet (is HTTPS enabled for your tailnet? "+
				"see https://tailscale.com/kb/1153/enabling-https): %w", err)
	}
	n.ln = ln

	srv := &http.Server{
		Handler:           n.withIdentity(handler),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: decision hooks and event streams are long-lived by
		// design.
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return srv.Serve(ln)
}

// Close shuts the node down.
func (n *Node) Close() error {
	var err error
	n.once.Do(func() {
		if n.ln != nil {
			_ = n.ln.Close()
		}
		err = n.srv.Close()
	})
	return err
}

// DNSName returns the node's MagicDNS name once it is up.
func (n *Node) DNSName(ctx context.Context) (string, error) {
	if n.lc == nil {
		return "", fmt.Errorf("node is not started")
	}
	st, err := n.lc.Status(ctx)
	if err != nil {
		return "", err
	}
	if st.Self == nil || st.Self.DNSName == "" {
		return "", fmt.Errorf("no MagicDNS name yet")
	}
	return strings.TrimSuffix(st.Self.DNSName, "."), nil
}

// Status describes the node for `muxscale status` and `doctor`.
type Status struct {
	Hostname     string   `json:"hostname"`
	DNSName      string   `json:"dns_name,omitempty"`
	TailnetIPs   []string `json:"tailnet_ips,omitempty"`
	BackendState string   `json:"backend_state"`
	AuthURL      string   `json:"auth_url,omitempty"`
	Port         int      `json:"port"`
}

// Status reports the node's current state.
func (n *Node) Status(ctx context.Context) (Status, error) {
	s := Status{Hostname: n.cfg.Hostname, Port: n.cfg.Port, AuthURL: n.AuthURL()}
	if n.lc == nil {
		// Up() has not returned yet. If a login URL was captured, that is
		// precisely what the user needs to see right now.
		s.BackendState = ipn.NeedsLogin.String()
		if s.AuthURL == "" {
			s.BackendState = ipn.NoState.String()
		}
		return s, nil
	}
	st, err := n.lc.Status(ctx)
	if err != nil {
		return s, err
	}
	s.BackendState = st.BackendState
	if st.AuthURL != "" {
		s.AuthURL = st.AuthURL
	}
	if st.Self != nil {
		s.DNSName = strings.TrimSuffix(st.Self.DNSName, ".")
		for _, ip := range st.Self.TailscaleIPs {
			s.TailnetIPs = append(s.TailnetIPs, ip.String())
		}
	}
	return s, nil
}

// Identity is a verified tailnet caller.
type Identity struct {
	// LoginName is the tailnet user, e.g. "alice@example.com". This is
	// cryptographically attested by Tailscale, not self-asserted by the client,
	// which is what makes it usable as the sole authorization input.
	LoginName   string
	DisplayName string
	NodeName    string
	IsTagged    bool
}

type identityKey struct{}

// IdentityFrom returns the verified caller identity for a request.
func IdentityFrom(r *http.Request) (Identity, bool) {
	id, ok := r.Context().Value(identityKey{}).(Identity)
	return id, ok
}

// withIdentity attaches the verified tailnet identity to each request.
//
// A request whose identity cannot be established is refused outright rather
// than treated as anonymous: this endpoint can send input to a coding agent,
// so "we could not tell who this is" must never mean "proceed".
func (n *Node) withIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, err := n.lc.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil || who.UserProfile == nil {
			http.Error(w, "could not verify your tailnet identity", http.StatusForbidden)
			return
		}
		id := Identity{
			LoginName:   who.UserProfile.LoginName,
			DisplayName: who.UserProfile.DisplayName,
		}
		if who.Node != nil {
			id.NodeName = strings.TrimSuffix(who.Node.Name, ".")
			id.IsTagged = who.Node.IsTagged()
		}
		ctx := contextWithIdentity(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func contextWithIdentity(ctx context.Context, id Identity) context.Context {
	return contextWithValue(ctx, identityKey{}, id)
}

// contextWithValue exists so the key type stays unexported.
func contextWithValue(ctx context.Context, k, v any) context.Context {
	return context.WithValue(ctx, k, v)
}
