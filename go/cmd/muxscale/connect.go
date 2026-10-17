package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"rsc.io/qr"
)

// cmdQR prints a QR code that connects the phone app to this daemon.
//
// The address was the one step of setup that had to be typed on a phone: a
// MagicDNS name, a tailnet suffix nobody remembers, and a port. Scanning it
// replaces all of that. The code carries the address and nothing else -- no
// token, no key -- because over the tailnet there is no secret to hand over:
// Tailscale attests which device is calling, and the daemon's ACL decides.
// A screenshot of this code therefore lets nobody in who was not already on
// the tailnet and on the allow list.
func cmdQR(args []string) error {
	for _, a := range args {
		switch a {
		case "--link", "--json":
		default:
			return fmt.Errorf("usage: muxscale qr [--link|--json]")
		}
	}

	resp, err := apiRequest("GET", "/v1/tailnet", nil)
	if err != nil {
		return daemonDown(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		// Not "tailnet is off": the route itself is missing, which means the
		// daemon answering is a build from before runtime tailnet control.
		return fmt.Errorf("the running daemon predates `muxscale qr`; restart it with this build " +
			"(see `muxscale adopt | grep wrap` first -- only `muxscale run` sessions die with it)")
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	var tn tailnetView
	if err := json.NewDecoder(resp.Body).Decode(&tn); err != nil {
		return err
	}

	base, err := tailnetBaseURL(tn)
	if err != nil {
		return err
	}
	link := connectLink(base)

	switch {
	case hasFlag(args, "--link"):
		fmt.Println(link)
		return nil
	case hasFlag(args, "--json"):
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"url": base, "link": link})
	}

	if err := renderQR(os.Stdout, link); err != nil {
		return err
	}
	fmt.Printf("\n  Scan with your phone's camera, or in the app: Settings → Scan QR code.\n")
	fmt.Printf("  %s\n\n", base)

	// The code is useless until the phone's identity is allowed, and the
	// failure mode is a 403 on the phone with the fix printed only here.
	var acl struct {
		Allowed []string `json:"allowed"`
	}
	if err := apiGet("/v1/acl", &acl); err == nil && len(acl.Allowed) == 0 {
		fmt.Println("  Nobody is permitted to connect yet. Allow your Tailscale login first:")
		fmt.Println("    muxscale acl allow you@example.com")
		fmt.Println()
	}
	return nil
}

// tailnetBaseURL is where the phone should connect, or why there is nowhere.
func tailnetBaseURL(tn tailnetView) (string, error) {
	if !tn.Enabled {
		return "", fmt.Errorf("the tailnet listener is off, so there is nothing for a phone to reach; " +
			"run `muxscale tailnet on --system` (no restart needed)")
	}
	if tn.URL != "" {
		return tn.URL, nil
	}
	if t := tn.Status; t != nil {
		if t.AuthURL != "" && t.BackendState != "Running" {
			return "", fmt.Errorf("the tailnet node needs a login before it has an address:\n  %s", t.AuthURL)
		}
		if t.DNSName != "" {
			return fmt.Sprintf("https://%s:%d", strings.TrimSuffix(t.DNSName, "."), t.Port), nil
		}
	}
	return "", fmt.Errorf("the tailnet listener is starting and has no address yet; try again in a moment")
}

// connectLink is the deep link the app opens. Its shape is mirrored in
// app/src/lib/connect.ts; the two must agree.
func connectLink(base string) string {
	return "muxscale://connect?url=" + url.QueryEscape(base)
}

// renderQR draws a QR code with half-block characters, two modules per
// character cell, so it fits a normal terminal. Colours are set explicitly
// (black on white) because a code drawn in "the terminal's foreground" is
// inverted on a dark theme, and plenty of phone scanners refuse inverted codes.
func renderQR(w io.Writer, text string) error {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return err
	}
	const quiet = 2 // modules of white border; the spec asks for 4, scanners manage with 2
	n := code.Size
	dark := func(x, y int) bool {
		if x < 0 || y < 0 || x >= n || y >= n {
			return false
		}
		return code.Black(x, y)
	}
	var b strings.Builder
	for y := -quiet; y < n+quiet; y += 2 {
		b.WriteString("  \x1b[30;47m")
		for x := -quiet; x < n+quiet; x++ {
			top, bottom := dark(x, y), dark(x, y+1)
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\x1b[0m\n")
	}
	_, err = io.WriteString(w, b.String())
	return err
}
