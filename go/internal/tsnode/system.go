package tsnode

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"tailscale.com/client/local"
)

// SystemNode serves the daemon on the machine's existing Tailscale node,
// instead of joining the tailnet as one of its own.
//
// The embedded tsnet node is still the default and still the better shape: it
// is a separate node, so a tailnet ACL can grant a phone access to muxscale
// without granting it everything else the workstation exposes. This exists
// because tsnet needs to register a new node with Tailscale's control plane,
// and that is a thing that can fail -- a tailnet requiring device approval, a
// network that will not complete the handshake, a machine limit already
// reached. When the machine plainly has a working Tailscale, refusing to use it
// is a bad answer.
//
// The security properties are the same in the ways that matter. Identity still
// comes from Tailscale's WhoIs over the local API, which is attested rather
// than self-asserted, and muxscale's own ACL is still the second, independent
// gate. What is given up is separation: this listens on the host's tailnet
// address, so a tailnet ACL cannot distinguish muxscale from anything else the
// host serves. muxscale's ACL still can.
//
// Transport is plain HTTP. There is no TLS to add here: the address only exists
// inside the tailnet and every byte reaching it has already been through
// WireGuard. tsnet's listener uses TLS only because Tailscale issues it a
// certificate for its own name, and even there the encryption is redundant.
type SystemNode struct {
	lc   *local.Client
	port int
	ln   net.Listener
}

// SystemConfig configures the system-Tailscale listener.
type SystemConfig struct {
	// Port to listen on across the host's tailnet addresses.
	Port int
}

// NewSystem prepares a listener on the machine's existing Tailscale node.
//
// It fails if Tailscale is not running or not logged in, and says which,
// because those need different things done about them.
func NewSystem(ctx context.Context, cfg SystemConfig) (*SystemNode, error) {
	if cfg.Port == 0 {
		cfg.Port = 4443
	}
	lc := &local.Client{}

	st, err := lc.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the system Tailscale (is it running?): %w", err)
	}
	if st.BackendState != "Running" {
		return nil, fmt.Errorf(
			"the system Tailscale is %s, not Running; log in with `tailscale up` first",
			st.BackendState)
	}
	if st.Self == nil || len(st.Self.TailscaleIPs) == 0 {
		return nil, fmt.Errorf("the system Tailscale has no tailnet address yet")
	}

	return &SystemNode{lc: lc, port: cfg.Port}, nil
}

// Start listens and serves until ctx is done.
//
// It binds the host's tailnet addresses specifically, never all interfaces. The
// difference is the whole point: on 0.0.0.0 this would answer on café wifi.
func (s *SystemNode) Start(ctx context.Context, handler http.Handler) error {
	st, err := s.lc.Status(ctx)
	if err != nil {
		return err
	}
	if st.Self == nil || len(st.Self.TailscaleIPs) == 0 {
		return fmt.Errorf("the system Tailscale has no tailnet address")
	}

	// IPv4 is enough and keeps the failure modes simple; the v6 address routes
	// to the same node for any client that has both.
	var addr string
	for _, ip := range st.Self.TailscaleIPs {
		if ip.Is4() {
			addr = net.JoinHostPort(ip.String(), fmt.Sprint(s.port))
			break
		}
	}
	if addr == "" {
		addr = net.JoinHostPort(st.Self.TailscaleIPs[0].String(), fmt.Sprint(s.port))
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on the tailnet address %s: %w", addr, err)
	}
	s.ln = ln

	srv := &http.Server{
		Handler:           WithIdentity(s.lc, handler),
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

// Close stops the listener.
func (s *SystemNode) Close() error {
	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}

// Status reports the host node's state, shaped like the tsnet one so `status`
// and `doctor` do not have to care which mode is in use.
func (s *SystemNode) Status(ctx context.Context) (Status, error) {
	out := Status{Port: s.port}
	st, err := s.lc.Status(ctx)
	if err != nil {
		return out, err
	}
	out.BackendState = st.BackendState
	if st.Self != nil {
		out.DNSName = strings.TrimSuffix(st.Self.DNSName, ".")
		out.Hostname = st.Self.HostName
		for _, ip := range st.Self.TailscaleIPs {
			out.TailnetIPs = append(out.TailnetIPs, ip.String())
		}
	}
	return out, nil
}
