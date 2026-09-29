package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/muxscale/muxscale/go/internal/tsnode"
)

// EnableTailnet brings up the Tailscale listener on a running daemon.
//
// This exists because the listener used to be start-only, and turning it on
// meant restarting the daemon -- which kills every session muxscale holds the
// PTY for. Sessions in a terminal, under tmux, or adopted through a terminal
// emulator survive a restart, but "restart the daemon to enable the tailnet"
// is still the wrong shape: the daemon is the thing that is watching, and it
// should not have to blink to start serving.
//
// useSystem serves on the machine's existing Tailscale node instead of
// joining the tailnet as one of its own; see tsnode.SystemNode for the tradeoff.
func (d *Daemon) EnableTailnet(ctx context.Context, useSystem bool) (string, error) {
	d.tailnetMu.Lock()
	defer d.tailnetMu.Unlock()

	if d.tailnetCancel != nil {
		return d.tailnetURL, fmt.Errorf(
			"the tailnet listener is already running (%s at %s); "+
				"turn it off first with `muxscale tailnet off`", d.tailnetMode, d.tailnetURL)
	}
	if d.mux == nil || d.stateDir == "" {
		return "", fmt.Errorf("the daemon is not running yet")
	}

	// Parented to the daemon, never to the caller. See runCtx.
	parent := d.runCtx
	if parent == nil {
		parent = ctx
	}
	tctx, cancel := context.WithCancel(parent)
	start := d.tailnetStarter
	if start == nil {
		start = d.startTailnet
	}
	url, err := start(tctx, d.stateDir, d.mux, useSystem)
	if err != nil {
		cancel()
		d.node, d.sysnode = nil, nil
		return "", err
	}
	d.tailnetCancel = cancel
	d.tailnetURL = url
	d.tailnetMode = "tsnet"
	if useSystem {
		d.tailnetMode = "system"
	}
	_ = d.store.Audit("cli", "tailnet_enabled", "", d.tailnetMode)
	return url, nil
}

// DisableTailnet stops the Tailscale listener. The loopback listener, the
// sessions, and everything already discovered are untouched.
func (d *Daemon) DisableTailnet() error {
	d.tailnetMu.Lock()
	defer d.tailnetMu.Unlock()

	if d.tailnetCancel == nil {
		return fmt.Errorf("the tailnet listener is not running")
	}
	d.tailnetCancel()
	d.tailnetCancel = nil

	var err error
	if d.sysnode != nil {
		err = d.sysnode.Close()
		d.sysnode = nil
	}
	if d.node != nil {
		if e := d.node.Close(); err == nil {
			err = e
		}
		d.node = nil
	}
	d.tailnetURL, d.tailnetMode = "", ""
	_ = d.store.Audit("cli", "tailnet_disabled", "", "")
	return err
}

// TailnetEnabled reports whether the listener is up, and in which mode.
func (d *Daemon) TailnetEnabled() (mode string, url string, ok bool) {
	d.tailnetMu.Lock()
	defer d.tailnetMu.Unlock()
	return d.tailnetMode, d.tailnetURL, d.tailnetCancel != nil
}

// startTailnet brings up the Tailscale node that phones connect to.
//
// Two independent gates guard it: Tailscale attests the caller's identity
// (they cannot forge it), and muxscale's own ACL decides whether that identity
// may reach this daemon. Tailnet ACLs alone are not enough -- a fresh tailnet
// lets a user's devices reach each other on every port, and a user on a company
// tailnet may not be able to change that.
//
// The returned URL is best-effort: a tsnet node does not know its own name
// until it has registered, so that path returns "" and the URL is reported by
// reportTailnet once it exists.
func (d *Daemon) startTailnet(ctx context.Context, stateDir string, mux http.Handler, useSystem bool) (string, error) {
	acl, err := tsnode.LoadACL(stateDir)
	if err != nil {
		return "", err
	}
	d.acl = acl

	if useSystem {
		return d.startSystemTailnet(ctx, acl, mux)
	}

	node, err := tsnode.New(tsnode.Config{
		Hostname: d.cfg.TailnetHostname,
		StateDir: stateDir,
		AuthKey:  d.cfg.TailnetAuthKey,
		Port:     d.cfg.TailnetPort,
		Logf:     tsnetLogf(d.log),
	})
	if err != nil {
		return "", err
	}
	d.node = node

	guarded := acl.Middleware(mux, func(id tsnode.Identity, r *http.Request) {
		// Every refusal is audited: an unexpected identity knocking is exactly
		// what someone reviewing an incident needs to see.
		_ = d.store.Audit("tailnet", "connection_refused", "",
			fmt.Sprintf("identity=%s node=%s path=%s", id.LoginName, id.NodeName, r.URL.Path))
		d.log.Warn("refused a tailnet connection", "identity", id.LoginName, "node", id.NodeName)
	})

	go func() {
		if err := node.Start(ctx, guarded); err != nil && ctx.Err() == nil {
			d.log.Error("tailnet listener stopped", "err", err)
		}
	}()

	go d.reportTailnet(ctx, node)
	return "", nil
}

// startSystemTailnet serves on the machine's existing Tailscale node.
//
// Same two gates as the tsnet path: Tailscale attests who is calling, and
// muxscale's ACL decides whether they may. The hook endpoint is untouched and
// stays loopback-only -- this adds a second, authenticated listener rather than
// widening the first one.
func (d *Daemon) startSystemTailnet(ctx context.Context, acl *tsnode.ACL, mux http.Handler) (string, error) {
	node, err := tsnode.NewSystem(ctx, tsnode.SystemConfig{Port: d.cfg.TailnetPort})
	if err != nil {
		return "", err
	}
	d.sysnode = node

	guarded := acl.Middleware(mux, func(id tsnode.Identity, r *http.Request) {
		_ = d.store.Audit("tailnet", "connection_refused", "",
			fmt.Sprintf("identity=%s node=%s path=%s", id.LoginName, id.NodeName, r.URL.Path))
		d.log.Warn("refused a tailnet connection", "identity", id.LoginName, "node", id.NodeName)
	})

	// Bind before serving, so the URL logged below is the one that works. The
	// scheme is not known until Tailscale has been asked for a certificate.
	if err := node.Listen(ctx); err != nil {
		return "", err
	}

	go func() {
		if err := node.Serve(ctx, guarded); err != nil && ctx.Err() == nil {
			d.log.Error("tailnet listener stopped", "err", err)
		}
	}()

	d.log.Info("tailnet listener ready (system Tailscale)", "url", node.URL())
	if err := node.CertError(); err != nil {
		// Serving plain HTTP is a working setup, but a client that assumes
		// https gets a bare TLS handshake failure and no way to know why. Say
		// it here, once, where it can be matched against that.
		d.log.Warn("serving without TLS: Tailscale would not issue a certificate "+
			"(enable HTTPS for the tailnet at https://tailscale.com/kb/1153) -- "+
			"use the http:// URL above",
			"err", err)
	}
	return node.URL(), nil
}

// tsnetLogf decides what to do with tsnet's own logging.
//
// Discarded by default, because tsnet emits several lines a second about DERP
// regions and netcheck results and none of it belongs in a user's terminal.
// Discarding it unconditionally was worse though: when the node fails to come
// up -- which is the one time anybody cares -- there is nothing at all to look
// at, and "backend_state: NoState" is the entire diagnosis available.
//
// MUXSCALE_TSNET_DEBUG=1 turns it back on.
//
// Logged at info, not debug, even though it plainly is debug output: the
// daemon's logger is built at info level, so a debug call is dropped and the
// environment variable silently does nothing -- which is how the first version
// of this shipped. Somebody who sets a variable named DEBUG has asked for the
// noise and should get it.
func tsnetLogf(log *slog.Logger) func(string, ...any) {
	if os.Getenv("MUXSCALE_TSNET_DEBUG") == "" {
		return func(string, ...any) {}
	}
	return func(format string, args ...any) {
		log.Info("tsnet: " + fmt.Sprintf(format, args...))
	}
}

// reportTailnet waits for the node to settle and says what happened.
//
// This polls rather than asking once. Asking once is what it used to do, and it
// could not work: it ran immediately after Start was handed to a goroutine, so
// the Tailscale client was always still nil, the status was always NoState with
// no URL and no name, and both branches were skipped. The daemon therefore
// never printed the login URL it exists to surface, and a first-time user was
// left with a node stuck in NoState and nothing telling them why.
func (d *Daemon) reportTailnet(ctx context.Context, node *tsnode.Node) {
	const interval = 2 * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()

	var lastURL string
	for {
		st, err := node.Status(ctx)
		switch {
		case err != nil:
			// Transient while the node starts. Keep waiting; ctx ends this.

		case st.DNSName != "":
			d.log.Info("tailnet listener ready",
				"url", fmt.Sprintf("https://%s:%d", st.DNSName, st.Port))
			return

		case st.AuthURL != "" && st.AuthURL != lastURL:
			// Logged on change rather than every tick: tsnet re-prints this
			// every few seconds and duplicating that buries everything else.
			lastURL = st.AuthURL
			d.log.Info("tailnet login required -- open this to finish connecting",
				"url", st.AuthURL)
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// TailnetStatus reports the node state for `status` and `doctor`.
func (d *Daemon) TailnetStatus(ctx context.Context) (tsnode.Status, bool) {
	d.tailnetMu.Lock()
	sysnode, node := d.sysnode, d.node
	d.tailnetMu.Unlock()

	if sysnode != nil {
		st, err := sysnode.Status(ctx)
		if err != nil {
			return tsnode.Status{}, false
		}
		return st, true
	}
	if node == nil {
		return tsnode.Status{}, false
	}
	st, err := node.Status(ctx)
	if err != nil {
		return tsnode.Status{}, false
	}
	return st, true
}

// ACL exposes the allowlist for the CLI.
func (d *Daemon) ACL() *tsnode.ACL { return d.acl }
