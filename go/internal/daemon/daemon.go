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
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
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
	cfg  Config
	reg  *Registry
	log  *slog.Logger
	srv  *http.Server
	addr string
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
	mux := http.NewServeMux()
	NewHookServer(d.reg, d.cfg.HookToken, d.cfg.HoldTime, d.log).Routes(mux)
	d.routes(mux)

	ln, err := net.Listen("tcp", d.cfg.ListenLocal)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", d.cfg.ListenLocal, err)
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

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return d.srv.Shutdown(shutCtx)
	}
}

// Addr is the bound address, valid after Run has started.
func (d *Daemon) Addr() string { return d.addr }

// routes registers the local control API used by the CLI.
func (d *Daemon) routes(mux *http.ServeMux) {
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
