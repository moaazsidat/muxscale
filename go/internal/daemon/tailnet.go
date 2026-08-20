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

// startTailnet brings up the Tailscale node that phones connect to.
//
// Two independent gates guard it: Tailscale attests the caller's identity
// (they cannot forge it), and muxscale's own ACL decides whether that identity
// may reach this daemon. Tailnet ACLs alone are not enough -- a fresh tailnet
// lets a user's devices reach each other on every port, and a user on a company
// tailnet may not be able to change that.
func (d *Daemon) startTailnet(ctx context.Context, stateDir string, mux http.Handler) error {
	acl, err := tsnode.LoadACL(stateDir)
	if err != nil {
		return err
	}
	d.acl = acl

	if d.cfg.UseSystemTS {
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
		return err
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
	return nil
}

// startSystemTailnet serves on the machine's existing Tailscale node.
//
// Same two gates as the tsnet path: Tailscale attests who is calling, and
// muxscale's ACL decides whether they may. The hook endpoint is untouched and
// stays loopback-only -- this adds a second, authenticated listener rather than
// widening the first one.
func (d *Daemon) startSystemTailnet(ctx context.Context, acl *tsnode.ACL, mux http.Handler) error {
	node, err := tsnode.NewSystem(ctx, tsnode.SystemConfig{Port: d.cfg.TailnetPort})
	if err != nil {
		return err
	}
	d.sysnode = node

	guarded := acl.Middleware(mux, func(id tsnode.Identity, r *http.Request) {
		_ = d.store.Audit("tailnet", "connection_refused", "",
			fmt.Sprintf("identity=%s node=%s path=%s", id.LoginName, id.NodeName, r.URL.Path))
		d.log.Warn("refused a tailnet connection", "identity", id.LoginName, "node", id.NodeName)
	})

	go func() {
		if err := node.Start(ctx, guarded); err != nil && ctx.Err() == nil {
			d.log.Error("tailnet listener stopped", "err", err)
		}
	}()

	if st, err := node.Status(ctx); err == nil && st.DNSName != "" {
		// http, not https: the address exists only inside the tailnet and every
		// byte reaching it has already been through WireGuard.
		d.log.Info("tailnet listener ready (system Tailscale)",
			"url", fmt.Sprintf("http://%s:%d", st.DNSName, st.Port))
	}
	return nil
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
	if d.sysnode != nil {
		st, err := d.sysnode.Status(ctx)
		if err != nil {
			return tsnode.Status{}, false
		}
		return st, true
	}
	if d.node == nil {
		return tsnode.Status{}, false
	}
	st, err := d.node.Status(ctx)
	if err != nil {
		return tsnode.Status{}, false
	}
	return st, true
}

// ACL exposes the allowlist for the CLI.
func (d *Daemon) ACL() *tsnode.ACL { return d.acl }
