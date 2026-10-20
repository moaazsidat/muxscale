package tsnode

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
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
// Transport is HTTPS where Tailscale will issue a certificate for the node's
// name, and plain HTTP where it will not.
//
// The encryption is redundant either way -- the address only exists inside the
// tailnet and every byte reaching it has already been through WireGuard. TLS is
// here because clients assume it. A bare hostname typed into the app becomes an
// https URL, browsers upgrade, and a plain-HTTP listener answers all of them
// with a TLS handshake failure that says nothing about what is wrong. Serving
// the certificate Tailscale is already willing to issue costs one call and
// removes a whole category of confusing failure.
type SystemNode struct {
	lc   *local.Client
	port int
	ln   net.Listener

	// tls records which scheme Start settled on, so the daemon can print a URL
	// that actually works rather than one that looks right.
	tls     bool
	certErr error
	dnsName string
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

// Listen binds the tailnet address and settles on a scheme.
//
// Separate from Serve so the caller can report a working URL before anything is
// served. Doing both in one call meant the scheme was decided on a background
// goroutine while the caller was already logging a guess at it -- wrong half the
// time, and a data race the rest.
//
// It binds the host's tailnet addresses specifically, never all interfaces. The
// difference is the whole point: on 0.0.0.0 this would answer on café wifi.
func (s *SystemNode) Listen(ctx context.Context) error {
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
	if tlsLn, ok := s.tlsListener(ctx, ln, st); ok {
		ln = tlsLn
		s.tls = true
	}
	s.ln = ln
	s.dnsName = strings.TrimSuffix(st.Self.DNSName, ".")
	return nil
}

// Serve handles requests until ctx is done. Listen must have run first.
func (s *SystemNode) Serve(ctx context.Context, handler http.Handler) error {
	if s.ln == nil {
		return fmt.Errorf("Serve called before Listen")
	}
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
	return srv.Serve(s.ln)
}

// tlsListener wraps the listener in TLS if Tailscale will issue a certificate
// for this node, and reports whether it did.
//
// The certificate is fetched once up front rather than left to the handshake.
// tls.Config.GetCertificate is called lazily, so an unavailable certificate --
// HTTPS not enabled for the tailnet, most commonly -- surfaces to the client as
// an opaque handshake error with nothing in the daemon's log to match it
// against. Asking now means the daemon can say plainly which scheme it ended up
// serving, and the fetch warms tailscaled's cache for the first real request.
func (s *SystemNode) tlsListener(ctx context.Context, ln net.Listener, st *ipnstate.Status) (net.Listener, bool) {
	name := strings.TrimSuffix(st.Self.DNSName, ".")
	if name == "" {
		return nil, false
	}

	// Issuing a certificate for the first time talks to Let's Encrypt, which is
	// slower than anything else here; renewals come from cache.
	probeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if _, _, err := s.lc.CertPair(probeCtx, name); err != nil {
		s.certErr = err
		return nil, false
	}

	return tls.NewListener(ln, &tls.Config{
		GetCertificate: s.lc.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}), true
}

// URL is where a client should point. Valid after Listen.
func (s *SystemNode) URL() string {
	scheme := "http"
	if s.tls {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, s.dnsName, s.port)
}

// CertError is why TLS was not used, if it was not. Empty when serving HTTPS.
func (s *SystemNode) CertError() error { return s.certErr }

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
	out.Login = selfLogin(st)
	out.HTTPSProblem = httpsProblem(st)
	if out.HTTPSProblem == "" && s.certErr != nil {
		// The tailnet allows certificates but this one was not issued: a
		// Tailscale too old to fetch one, or control unreachable just then.
		out.HTTPSProblem = "Tailscale would not issue this machine a certificate (" +
			s.certErr.Error() + "). Check HTTPS is on at " + DNSAdminURL +
			", then restart muxscale (`muxscale up`)"
	}
	return out, nil
}
