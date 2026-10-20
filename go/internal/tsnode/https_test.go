package tsnode

import (
	"strings"
	"testing"

	"tailscale.com/ipn/ipnstate"
)

func TestHTTPSProblem(t *testing.T) {
	running := func(magic bool, certs ...string) *ipnstate.Status {
		return &ipnstate.Status{
			BackendState:   "Running",
			Self:           &ipnstate.PeerStatus{DNSName: "box.tail1234.ts.net."},
			CurrentTailnet: &ipnstate.TailnetStatus{MagicDNSEnabled: magic},
			CertDomains:    certs,
		}
	}
	if p := httpsProblem(running(true, "box.tail1234.ts.net")); p != "" {
		t.Errorf("ready tailnet: %q", p)
	}
	if p := httpsProblem(running(true)); !strings.Contains(p, "Enable HTTPS") {
		t.Errorf("no certificates: %q", p)
	}
	if p := httpsProblem(running(false)); !strings.Contains(p, "MagicDNS") {
		t.Errorf("no MagicDNS: %q", p)
	}
	if p := httpsProblem(&ipnstate.Status{BackendState: "NeedsLogin"}); p != "" {
		t.Errorf("not up yet is not a verdict: %q", p)
	}
}
