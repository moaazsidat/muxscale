package daemon

import (
	"context"
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
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
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
	HookToken   string
	HoldTime    time.Duration
	StateDir    string
	Log         *slog.Logger

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
}

// DefaultStateDir follows platform convention.
func DefaultStateDir() (string, error) {
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
	cfg   Config
	reg   *Registry
	mgr   *Manager
	store *store.Store
	node  *tsnode.Node
	acl   *tsnode.ACL
	log   *slog.Logger
	srv   *http.Server
	addr  string
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
	d.mgr = NewManager(d.reg, d.cfg.Adapters, st, d.log)

	if n, err := d.mgr.Restore(); err != nil {
		d.log.Warn("restoring sessions failed", "err", err)
	} else if n > 0 {
		d.log.Info("restored sessions from disk", "count", n)
	}

	stopScan := make(chan struct{})
	defer close(stopScan)
	go d.mgr.DiscoverLoop(stopScan, 3*time.Second)

	mux := http.NewServeMux()
	hookSrv := NewHookServer(d.reg, d.cfg.HookToken, d.cfg.HoldTime, d.log)
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
	// daemon is fully usable locally without any Tailscale setup at all.
	if d.cfg.Tailnet {
		if err := d.startTailnet(ctx, stateDir, mux); err != nil {
			// A tailnet failure must not take down local monitoring, which is
			// the part that works with no network at all.
			d.log.Error("tailnet listener unavailable; local access still works", "err", err)
		}
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d.mgr.Shutdown(3 * time.Second)
		return d.srv.Shutdown(shutCtx)
	}
}

// Addr is the bound address, valid after Run has started.
func (d *Daemon) Addr() string { return d.addr }

// routes registers the local control API used by the CLI.
func (d *Daemon) routes(mux *http.ServeMux) {
	// Terminal-shaped endpoints (shell, resize, grid stream) live in screen.go.
	d.screenRoutes(mux)

	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"sessions": d.reg.List()})
	})

	mux.HandleFunc("/v1/approvals", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"approvals": d.reg.PendingApprovals()})
	})

	// POST /v1/approvals/decide {"id":..., "behavior":"allow"|"deny"}
	mux.HandleFunc("/v1/approvals/decide", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("/v1/sessions/run", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("/v1/sessions/input", func(w http.ResponseWriter, r *http.Request) {
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

	// POST /v1/sessions/kill {"session_id":"..."}
	mux.HandleFunc("/v1/sessions/kill", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("/v1/sessions/transcript", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("session")
		since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
		events, err := d.store.Transcript(id, since, 500)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"events": events})
	})

	// GET /v1/sessions/screen?session=ID[&format=text]
	mux.HandleFunc("/v1/sessions/screen", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{
			"local_addr": d.addr,
			"hold":       d.cfg.HoldTime.String(),
			"sessions":   len(d.reg.List()),
			"approvals":  len(d.reg.PendingApprovals()),
			"db":         d.store.Path(),
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

	// GET  /v1/acl              list
	// POST /v1/acl {"allow"|"deny": "user@example.com"}
	//
	// Deliberately local-only: the ACL is reachable through the loopback API,
	// never through the tailnet listener it guards. A remote caller must never
	// be able to grant itself access.
	mux.HandleFunc("/v1/acl", func(w http.ResponseWriter, r *http.Request) {
		if d.acl == nil {
			http.Error(w, "tailnet is not enabled (start the daemon with --tailnet)", http.StatusNotFound)
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
	mux.HandleFunc("/v1/audit", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		entries, err := d.store.AuditLog(limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"entries": entries})
	})

	// Server-sent events for `muxscale watch`.
	mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
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

		enc := json.NewEncoder(w)
		for {
			select {
			case <-r.Context().Done():
				return
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
