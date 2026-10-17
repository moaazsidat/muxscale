package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/adapter/claudecode"
	"github.com/muxscale/muxscale/go/internal/attach"
	"github.com/muxscale/muxscale/go/internal/protocol"
	"github.com/muxscale/muxscale/go/internal/store"
	"github.com/muxscale/muxscale/go/internal/tsnode"
)

// Config configures the daemon.
type Config struct {
	// ListenLocal is the loopback address for hook callbacks and the local
	// control API. Loopback only, always: binding this to 0.0.0.0 would expose
	// an unauthenticated-by-network-position approval endpoint.
	ListenLocal string

	// LocalToken authenticates callers on the loopback listener: the CLI, and
	// the agent hooks. Callers arriving over the tailnet are authenticated by
	// Tailscale identity instead and never see it.
	LocalToken string

	HoldTime time.Duration
	StateDir string
	Log      *slog.Logger

	// Adapters is the set of agent integrations this daemon can identify and
	// talk to. Injected rather than constructed here so tests can run with one
	// adapter, or none, and so the built-in set is chosen in exactly one place.
	// nil falls back to binary-name identification.
	Adapters *adapter.Registry

	// Tailnet enables the Tailscale listener that phones connect to. Off by
	// default: the daemon is fully usable locally with no Tailscale setup.
	Tailnet         bool
	TailnetHostname string
	TailnetAuthKey  string
	TailnetPort     int

	// UseSystemTS serves on the machine's existing Tailscale node instead of
	// joining the tailnet as a new one. See tsnode.SystemNode for the tradeoff;
	// in short, it gives up per-service tailnet ACLs and works on machines
	// where registering a new node does not.
	UseSystemTS bool
}

// DefaultStateDir follows platform convention.
//
// MUXSCALE_STATE_DIR overrides it. The daemon and the CLI both read it, so a
// second daemon can run alongside the first -- on another port, with its own
// database and token -- which is what makes a change testable without
// restarting, or even touching, the daemon someone is already relying on.
func DefaultStateDir() (string, error) {
	if x := os.Getenv("MUXSCALE_STATE_DIR"); x != "" {
		return x, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "muxscale"), nil
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "muxscale"), nil
	}
	return filepath.Join(home, ".local", "state", "muxscale"), nil
}

// Daemon is the long-lived process.
type Daemon struct {
	cfg     Config
	reg     *Registry
	mgr     *Manager
	store   *store.Store
	node    *tsnode.Node
	sysnode *tsnode.SystemNode
	acl     *tsnode.ACL
	log     *slog.Logger
	srv     *http.Server
	addr    string

	// mux and stateDir are kept so the tailnet listener can be brought up
	// after Run has started. The listener used to be start-only, so turning it
	// on meant restarting the daemon -- which kills every session muxscale
	// owns the PTY for. Enabling it at runtime is what makes "start the daemon
	// now, decide about the tailnet later" a safe order to do things in.
	mux      http.Handler
	stateDir string

	// runCtx is the daemon's own lifetime. A listener enabled by an HTTP
	// request must not be parented to that request's context: the request
	// context is cancelled the moment the handler returns, so the listener
	// would bind, report success, and be torn down before the caller could
	// connect to it. That is exactly what the first version did.
	runCtx context.Context

	// tailnetMu guards the listener's lifecycle and the fields describing it.
	// Held across a start or stop, which can take a second or two: a status
	// request that raced a stop would otherwise read a half-cleared node.
	tailnetMu     sync.Mutex
	tailnetCancel context.CancelFunc
	tailnetMode   string // "tsnet" or "system"; empty when no listener is up
	tailnetURL    string

	// tailnetStarter is the seam tests use to drive the listener lifecycle
	// without a real Tailscale. Nil means startTailnet.
	tailnetStarter func(context.Context, string, http.Handler, bool) (string, error)
}

// New builds a daemon.
func New(cfg Config) (*Daemon, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.ListenLocal == "" {
		cfg.ListenLocal = "127.0.0.1:4455"
	}
	if cfg.HoldTime == 0 {
		cfg.HoldTime = 120 * time.Second
	}
	if err := validateLoopback(cfg.ListenLocal); err != nil {
		return nil, err
	}
	return &Daemon{cfg: cfg, reg: NewRegistry(), log: cfg.Log}, nil
}

// Manager exposes session management, valid after Run has started.
func (d *Daemon) Manager() *Manager { return d.mgr }

// validateLoopback refuses any non-loopback bind address.
func validateLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf(
			"refusing to listen on %q: the hook endpoint must be loopback-only", addr)
	}
	return nil
}

// Registry exposes session state.
func (d *Daemon) Registry() *Registry { return d.reg }

// Run starts the daemon and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	stateDir := d.cfg.StateDir
	if stateDir == "" {
		var err error
		if stateDir, err = DefaultStateDir(); err != nil {
			return err
		}
	}
	st, err := store.Open(stateDir)
	if err != nil {
		return err
	}
	defer st.Close()
	d.store = st
	// Sessions recorded before the parser could tell a slash command or a
	// compaction summary from the human speaking still have them stored as
	// user messages. Read them again with the current rules, once.
	if n, err := st.RewriteEventsOnce("claudecode-cli-entries-v1", "claude-code",
		protocol.EventUserMessage, claudecode.Reclassify); err != nil {
		d.log.Warn("could not reclassify stored transcript entries", "err", err)
	} else if n > 0 {
		d.log.Info("reclassified stored transcript entries", "changed", n)
	}
	if n, err := rereadTranscriptsOnce(st); err != nil {
		d.log.Warn("could not re-read stored transcripts", "err", err)
	} else if n > 0 {
		d.log.Info("re-read transcripts the daemon had stored only in part", "sessions", n)
	}
	d.mgr = NewManager(d.reg, d.cfg.Adapters, st, d.log)
	d.stateDir = stateDir

	if n, err := d.mgr.Restore(); err != nil {
		d.log.Warn("restoring sessions failed", "err", err)
	} else if n > 0 {
		d.log.Info("restored sessions from disk", "count", n)
	}

	stopScan := make(chan struct{})
	defer close(stopScan)
	go d.mgr.DiscoverLoop(stopScan, 3*time.Second)

	mux := http.NewServeMux()
	d.mux = mux
	d.runCtx = ctx
	hookSrv := NewHookServer(d.reg, d.cfg.LocalToken, d.cfg.HoldTime, d.log)
	hookSrv.SetManager(d.mgr)
	hookSrv.Routes(mux)
	d.routes(mux)

	ln, lnErr := net.Listen("tcp", d.cfg.ListenLocal)
	if lnErr != nil {
		return fmt.Errorf("listening on %s: %w", d.cfg.ListenLocal, lnErr)
	}
	d.addr = ln.Addr().String()

	d.srv = &http.Server{
		Handler: mux,
		// No global write timeout: decision hooks are held open by design.
		// ReadHeaderTimeout still guards against a stuck client.
		ReadHeaderTimeout: 10 * time.Second,
	}

	d.log.Info("muxscale daemon listening", "addr", d.addr, "hold", d.cfg.HoldTime)

	errCh := make(chan error, 1)
	go func() {
		if err := d.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// The tailnet listener is what a phone connects to. It is optional so the
	// daemon is fully usable locally without any Tailscale setup at all, and
	// it can be turned on later with `muxscale connect` -- see
	// EnableTailnet for why that matters.
	//
	// With no flag, it comes back the way `muxscale connect` last left it.
	tailnet, system := d.cfg.Tailnet, d.cfg.UseSystemTS
	if !tailnet {
		switch mode, _ := st.Setting(settingTailnet); mode {
		case "system":
			tailnet, system = true, true
		case "tsnet":
			tailnet = true
		}
	}
	if tailnet {
		if _, err := d.EnableTailnet(ctx, system); err != nil {
			// A tailnet failure must not take down local monitoring, which is
			// the part that works with no network at all.
			d.log.Error("tailnet listener unavailable; local access still works", "err", err)
		} else if d.cfg.Tailnet {
			mode, _, _ := d.TailnetEnabled()
			d.rememberTailnet(mode)
		}
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.DisableTailnet()
		d.mgr.Shutdown(3 * time.Second)
		return d.srv.Shutdown(shutCtx)
	}
}

// rereadTranscriptsOnce rebuilds every stored Claude Code transcript from its
// file, once.
//
// Until events were stored directly, they reached the store through a feed
// that dropped whatever did not fit its buffer, so a session's history was
// kept only in part -- a few hundred events of a few thousand. The files
// still hold all of it. Run before any tailer starts, so none is appending
// while a transcript is swapped.
func rereadTranscriptsOnce(st *store.Store) (int, error) {
	const flag = "once:transcripts-reread-v1"
	if done, _ := st.Setting(flag); done != "" {
		return 0, nil
	}
	sessions, err := st.LoadSessions()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range sessions {
		if s.AgentID != "claude-code" || s.TranscriptPath == "" {
			continue
		}
		events, _, err := claudecode.ParseTranscriptFile(s.TranscriptPath)
		if err != nil {
			continue // the file is gone; what is stored is all there is
		}
		for i := range events {
			events[i].SessionID = s.ID
		}
		if err := st.ReplaceTranscript(s.ID, events); err != nil {
			return n, err
		}
		n++
	}
	return n, st.SetSetting(flag, time.Now().UTC().Format(time.RFC3339))
}

// Addr is the bound address, valid after Run has started.
func (d *Daemon) Addr() string { return d.addr }

// handle registers a control-API route behind authentication.
//
// Used instead of mux.HandleFunc for everything in the control API, so that
// adding a route without thinking about auth is not possible: the registration
// call is the auth decision.
func (d *Daemon) handle(mux *http.ServeMux, pattern string, fn http.HandlerFunc) {
	mux.Handle(pattern, d.requireAuth(fn))
}

// requireAuth admits a request that has proved itself one of the two ways
// there are to prove it.
//
// Over the tailnet, the listener has already established a Tailscale-attested
// identity and checked it against muxscale's ACL, so the presence of a verified
// identity in the context *is* the proof. The context value is set by that
// middleware and cannot be supplied by a caller.
//
// Over loopback, the proof is the bearer token, which anything running as this
// user can read from the state directory. That is the point: loopback is not a
// same-user boundary. Every local user can reach 127.0.0.1, as can a sandboxed
// process, and a browser can be walked onto it by DNS rebinding. Before this,
// any of them could approve a pending tool call, type into a running agent, or
// open a shell in one of its project directories -- while the hook endpoint,
// which can do strictly less, was the only thing asking for a token.
func (d *Daemon) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := tsnode.IdentityFrom(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		if d.tokenMatches(r) {
			next.ServeHTTP(w, r)
			return
		}
		d.log.Warn("control API request rejected: bad or missing token",
			"remote", r.RemoteAddr, "path", r.URL.Path)
		http.Error(w,
			"this endpoint needs the daemon's local token.\n"+
				"The muxscale CLI reads it automatically; other callers should send it as "+
				"`Authorization: Bearer <contents of hook-token in the state directory>`.",
			http.StatusUnauthorized)
	})
}

// tokenMatches compares the presented bearer token in constant time.
//
// An empty configured token denies rather than admits. The hook endpoint takes
// the opposite view -- see HookServer.authorized -- because a hook that cannot
// authenticate must fail open and let the agent proceed. Nothing here has that
// obligation, and a fail-open default on an authorization check is how an
// endpoint ends up unprotected because a config field was left blank.
func (d *Daemon) tokenMatches(r *http.Request) bool {
	if d.cfg.LocalToken == "" {
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(d.cfg.LocalToken)) == 1
}

// How often /v1/events writes a keepalive when there is nothing to send. Well
// under the app's silence timeout (45s), so one late ping is not a reconnect.
var eventsKeepalive = 15 * time.Second

// routes registers the local control API used by the CLI.
func (d *Daemon) routes(mux *http.ServeMux) {
	// Terminal-shaped endpoints (shell, resize, grid stream) live in screen.go.
	d.screenRoutes(mux)

	d.handle(mux, "/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"sessions": d.reg.List()})
	})

	d.handle(mux, "/v1/approvals", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"approvals": d.reg.PendingApprovals()})
	})

	// POST /v1/approvals/decide {"id":..., "behavior":"allow"|"deny"}
	d.handle(mux, "/v1/approvals/decide", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID       string `json:"id"`
			Behavior string `json:"behavior"`
			Reason   string `json:"reason"`
			By       string `json:"by"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.Behavior != "allow" && body.Behavior != "deny" {
			http.Error(w, `behavior must be "allow" or "deny"`, http.StatusBadRequest)
			return
		}
		ok := d.reg.ResolveApproval(body.ID, protocol.ApprovalDecision{
			Behavior: body.Behavior, Reason: body.Reason, DecidedBy: body.By,
		})
		writeJSON(w, map[string]bool{"delivered": ok})
	})

	// POST /v1/sessions/run  {"command":[...], "dir":"...", "name":"..."}
	d.handle(mux, "/v1/sessions/run", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Command []string `json:"command"`
			Dir     string   `json:"dir"`
			Name    string   `json:"name"`
			Cols    uint16   `json:"cols"`
			Rows    uint16   `json:"rows"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Command) == 0 {
			http.Error(w, "bad request: command required", http.StatusBadRequest)
			return
		}
		sess, err := d.mgr.Run(RunOptions{
			Command: body.Command, Dir: body.Dir, Name: body.Name,
			Cols: body.Cols, Rows: body.Rows,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, sess)
	})

	// POST /v1/sessions/input
	d.handle(mux, "/v1/sessions/input", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionID string `json:"session_id"`
			Text      string `json:"text"`
			Key       string `json:"key"`
			Submit    bool   `json:"submit"`
			Fence     struct {
				PromptID string `json:"prompt_id"`
				State    string `json:"state"`
			} `json:"fence"`
			By string `json:"by"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		actor := body.By
		if actor == "" {
			actor = "cli"
		}
		err := d.mgr.SendInput(body.SessionID, body.Text, attach.NamedKey(body.Key),
			body.Submit, attach.Fence{
				ExpectPromptID: body.Fence.PromptID, ExpectState: body.Fence.State,
			}, actor)
		if err != nil {
			// A failed fence is a normal, expected outcome, not a server fault:
			// the client is meant to show the new context and ask the human.
			var fe *attach.FenceError
			if errors.As(err, &fe) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": "FENCE_STALE", "detail": fe.Error(),
					"field": fe.Field, "want": fe.Want, "got": fe.Got,
				})
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"delivered": true})
	})

	// POST /v1/sessions/rename {"session_id":"...","name":"..."}; an empty
	// name clears it.
	d.handle(mux, "/v1/sessions/rename", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionID string `json:"session_id"`
			Name      string `json:"name"`
			By        string `json:"by"`
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		actor := body.By
		if actor == "" {
			actor = "cli"
		}
		sess, err := d.mgr.Rename(body.SessionID, body.Name, actor)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, sess)
	})

	// POST /v1/sessions/kill {"session_id":"..."}
	d.handle(mux, "/v1/sessions/kill", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionID string `json:"session_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := d.mgr.Kill(body.SessionID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"killed": true})
	})

	// GET /v1/sessions/transcript?session=ID&since=N
	//
	// GET /v1/sessions/transcript?session=ID&before=N
	//
	// With before, a page read backwards from the end instead: the newest
	// events when N is 0, else those before seq N, and whether older remain.
	// That is how a client opens a conversation, which it reads from its end.
	d.handle(mux, "/v1/sessions/transcript", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		id := q.Get("session")
		if q.Has("before") {
			before, _ := strconv.ParseUint(q.Get("before"), 10, 64)
			events, more, err := d.store.TranscriptBefore(id, before, 500)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"events": events, "more": more})
			return
		}
		since, _ := strconv.ParseUint(q.Get("since"), 10, 64)
		events, err := d.store.Transcript(id, since, 500)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"events": events})
	})

	// GET /v1/sessions/screen?session=ID[&format=text]
	d.handle(mux, "/v1/sessions/screen", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("session")
		if r.URL.Query().Get("format") == "text" {
			text, ok := d.mgr.ScreenText(id)
			if !ok {
				http.Error(w, "session has no screen (only wrapped sessions do)", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(text))
			return
		}
		snap, ok := d.mgr.Screen(id)
		if !ok {
			http.Error(w, "session has no screen (only wrapped sessions do)", http.StatusNotFound)
			return
		}
		writeJSON(w, snap)
	})

	// GET /v1/status
	d.handle(mux, "/v1/status", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{
			"local_addr": d.addr,
			"hold":       d.cfg.HoldTime.String(),
			"sessions":   len(d.reg.List()),
			"approvals":  len(d.reg.PendingApprovals()),
			"db":         d.store.Path(),
		}
		if mode, url, on := d.TailnetEnabled(); on {
			out["tailnet_enabled"] = true
			out["tailnet_mode"] = mode
			out["tailnet_url"] = url
		} else {
			out["tailnet_enabled"] = false
		}
		if st, ok := d.TailnetStatus(r.Context()); ok {
			out["tailnet"] = st
		} else {
			out["tailnet"] = nil
		}
		if d.acl != nil {
			out["acl"] = d.acl.List()
		}
		writeJSON(w, out)
	})

	// GET  /v1/tailnet                      status
	// POST /v1/tailnet {"enable":bool,"system":bool}
	//
	// The listener is startable at runtime so enabling it does not require
	// restarting the daemon, which would drop every PTY it owns. Still
	// local-only in the sense that matters: this route is on the loopback
	// listener, and the tailnet listener it starts is guarded by the ACL.
	d.handle(mux, "/v1/tailnet", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mode, url, on := d.TailnetEnabled()
			out := map[string]any{"enabled": on, "mode": mode, "url": url}
			if st, ok := d.TailnetStatus(r.Context()); ok {
				out["status"] = st
			}
			writeJSON(w, out)
			return
		}
		var body struct {
			Enable bool `json:"enable"`
			System bool `json:"system"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !body.Enable {
			if err := d.DisableTailnet(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			d.rememberTailnet("off")
			writeJSON(w, map[string]any{"enabled": false})
			return
		}
		url, err := d.EnableTailnet(r.Context(), body.System)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mode, _, _ := d.TailnetEnabled()
		d.rememberTailnet(mode)
		out := map[string]any{"enabled": true, "mode": mode, "url": url}
		if st, ok := d.TailnetStatus(r.Context()); ok {
			out["status"] = st
		}
		writeJSON(w, out)
	})

	// POST /v1/adopt
	//
	// Force a discovery pass and report what muxscale can reach. Adoption
	// itself is continuous; this is for the moment after a setting changed.
	d.handle(mux, "/v1/adopt", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.mgr.AdoptNow())
	})

	// GET  /v1/acl              list
	// POST /v1/acl {"allow"|"deny": "user@example.com"}
	//
	// Deliberately local-only: the ACL is reachable through the loopback API,
	// never through the tailnet listener it guards. A remote caller must never
	// be able to grant itself access.
	d.handle(mux, "/v1/acl", func(w http.ResponseWriter, r *http.Request) {
		if d.acl == nil {
			http.Error(w, "tailnet is not enabled (run `muxscale connect`)", http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{"allowed": d.acl.List()})
			return
		}
		var body struct {
			Allow string `json:"allow"`
			Deny  string `json:"deny"`
			Owner string `json:"owner"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var err error
		switch {
		case body.Owner != "":
			err = d.acl.SetOwner(body.Owner)
			_ = d.store.Audit("cli", "acl_set_owner", "", body.Owner)
		case body.Allow != "":
			err = d.acl.Allow(body.Allow)
			_ = d.store.Audit("cli", "acl_allow", "", body.Allow)
		case body.Deny != "":
			err = d.acl.Deny(body.Deny)
			_ = d.store.Audit("cli", "acl_deny", "", body.Deny)
		default:
			err = fmt.Errorf("nothing to do: give allow, deny, or owner")
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"allowed": d.acl.List()})
	})

	// GET /v1/audit
	d.handle(mux, "/v1/audit", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		entries, err := d.store.AuditLog(limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"entries": entries})
	})

	// Server-sent events for `muxscale ls -w`, and the phone.
	d.handle(mux, "/v1/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ch, cancel := d.reg.Subscribe()
		defer cancel()

		// A comment line on a quiet stream. Without it an idle connection
		// carries no bytes at all, so a client cannot tell "nothing has
		// happened" from "the network went away" -- a phone that changed
		// networks would show live forever and receive nothing. Clients treat
		// a long enough silence as a dead connection and reconnect.
		ping := time.NewTicker(eventsKeepalive)
		defer ping.Stop()

		enc := json.NewEncoder(w)
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ping.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case c, ok := <-ch:
				if !ok {
					return
				}
				fmt.Fprint(w, "data: ")
				if err := enc.Encode(c); err != nil {
					return
				}
				fmt.Fprint(w, "\n")
				flusher.Flush()
			}
		}
	})
}
