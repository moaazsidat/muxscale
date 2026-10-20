package main

import (
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestConnectLinkRoundTrips(t *testing.T) {
	base := "https://studio.example.ts.net:4443"
	link := connectLink(base)

	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	// Host "connect" is how expo-router sees muxscale://connect as the
	// /connect route; app/src/lib/connect.ts depends on exactly this shape.
	if u.Scheme != "muxscale" || u.Host != "connect" {
		t.Fatalf("link %q: scheme %q host %q", link, u.Scheme, u.Host)
	}
	if got := u.Query().Get("url"); got != base {
		t.Fatalf("url param = %q, want %q", got, base)
	}
}

func TestTailnetBaseURL(t *testing.T) {
	if _, err := tailnetBaseURL(tailnetView{}); err == nil || !strings.Contains(err.Error(), "muxscale connect") {
		t.Fatalf("off: want a hint to turn it on, got %v", err)
	}

	got, err := tailnetBaseURL(tailnetView{Enabled: true, URL: "https://a.ts.net:4443"})
	if err != nil || got != "https://a.ts.net:4443" {
		t.Fatalf("explicit URL: %q %v", got, err)
	}

	var tn tailnetView
	tn.Enabled = true
	tn.Status = &tailnetStatus{DNSName: "muxscale-studio.tail.ts.net.", Port: 4443, BackendState: "Running"}
	got, err = tailnetBaseURL(tn)
	if err != nil || got != "https://muxscale-studio.tail.ts.net:4443" {
		t.Fatalf("from status: %q %v", got, err)
	}

	tn.Status.BackendState, tn.Status.AuthURL = "NeedsLogin", "https://login.tailscale.com/a/x"
	if _, err := tailnetBaseURL(tn); err == nil || !strings.Contains(err.Error(), "login.tailscale.com") {
		t.Fatalf("needs login: want the auth URL, got %v", err)
	}
}

func TestRenderQRIsSquare(t *testing.T) {
	var b strings.Builder
	if err := renderQR(&b, connectLink("https://studio.example.ts.net:4443")); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	width := -1
	for i, l := range lines {
		l = strings.TrimPrefix(l, "  \x1b[30;47m")
		l = strings.TrimSuffix(l, "\x1b[0m")
		if w := utf8.RuneCountInString(l); width == -1 {
			width = w
		} else if w != width {
			t.Fatalf("line %d is %d wide, want %d", i, w, width)
		}
	}
	// Two module rows per line, so a square code is about half as tall as wide.
	if h := len(lines); h < width/2 || h > width/2+1 {
		t.Fatalf("%d lines for width %d: not square", h, width)
	}
}

// The app only speaks HTTPS: a tailnet without certificates must stop connect
// before it shows a code that cannot work.
func TestHTTPSProblemStopsConnect(t *testing.T) {
	var tn tailnetView
	if p := httpsProblem(tn); p != "" {
		t.Errorf("no status, no problem: %q", p)
	}
	tn.URL = "http://a.ts.net:4443"
	if p := httpsProblem(tn); !strings.Contains(p, "login.tailscale.com/admin/dns") {
		t.Errorf("plain http: %q", p)
	}
	tn.URL = "https://a.ts.net:4443"
	tn.Status = &tailnetStatus{HTTPSProblem: "HTTPS certificates are off"}
	if p := httpsProblem(tn); p != "HTTPS certificates are off" {
		t.Errorf("daemon's reason: %q", p)
	}
}
