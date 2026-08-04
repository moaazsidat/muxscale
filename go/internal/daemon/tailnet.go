package daemon

import (
	"context"
	"fmt"
	"net/http"

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

	node, err := tsnode.New(tsnode.Config{
		Hostname: d.cfg.TailnetHostname,
		StateDir: stateDir,
		AuthKey:  d.cfg.TailnetAuthKey,
		Port:     d.cfg.TailnetPort,
		Logf:     func(string, ...any) {}, // tsnet is chatty; keep it out of our log
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

	// Report the node's address once it settles, so the user has something to
	// point a phone at.
	go func() {
		st, err := node.Status(ctx)
		if err != nil {
			return
		}
		if st.AuthURL != "" {
			d.log.Info("tailnet login required", "url", st.AuthURL)
			return
		}
		if st.DNSName != "" {
			d.log.Info("tailnet listener ready",
				"url", fmt.Sprintf("https://%s:%d", st.DNSName, st.Port))
		}
	}()
	return nil
}

// TailnetStatus reports the node state for `status` and `doctor`.
func (d *Daemon) TailnetStatus(ctx context.Context) (tsnode.Status, bool) {
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
