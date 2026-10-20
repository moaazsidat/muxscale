package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"rsc.io/qr"
)

// cmdConnect serves the daemon to your phone and prints the code to scan.
//
// It does what used to take three commands -- `tailnet on`, `acl owner`, `qr`
// -- and each of them could be got wrong in a way that only showed on the
// phone. The listener comes up on the machine's own Tailscale when there is
// one, else on a node of muxscale's own; the daemon remembers it, so it comes
// back after a restart; and the person whose Tailscale login this machine
// uses is let in without being named.
//
// The code carries the address and nothing else -- no token, no key --
// because over the tailnet there is no secret to hand over: Tailscale attests
// which device is calling, and the daemon's ACL decides. A screenshot of this
// code therefore lets nobody in who was not already on the tailnet and on the
// allow list.
func cmdConnect(args []string) error {
	embedded := false
	for _, a := range args {
		switch a {
		case "off":
			return connectOff()
		case "--link", "--json":
		case "--embedded":
			embedded = true
		default:
			return fmt.Errorf("usage: muxscale connect [off] [--embedded] [--link|--json]")
		}
	}

	tn, err := getTailnet()
	if err != nil {
		return err
	}
	if !tn.Enabled {
		if tn, err = enableTailnet(!embedded); err != nil && !embedded {
			// No Tailscale app on this machine, or it is not logged in: run a
			// node of muxscale's own instead, which logs in through a link.
			fmt.Fprintf(os.Stderr, "Tailscale on this machine is not available (%v);\n"+
				"starting muxscale's own Tailscale node instead.\n\n", err)
			tn, err = enableTailnet(false)
		}
		if err != nil {
			return err
		}
	}

	// A node of its own has no name until it has registered, which can take
	// a few seconds, or a login.
	base, err := tailnetBaseURL(tn)
	for wait := 0; err != nil && wait < 15 && !strings.Contains(err.Error(), "login"); wait++ {
		time.Sleep(time.Second)
		if tn, err = getTailnet(); err != nil {
			return err
		}
		base, err = tailnetBaseURL(tn)
	}
	if err != nil {
		return fmt.Errorf("%w\nThen run `muxscale connect` again", err)
	}
	// Checked before the code is shown: the app only connects over HTTPS,
	// and a code that cannot work fails on the phone with a TLS error and no
	// hint of the fix, which is a setting in the tailnet's admin console.
	if p := httpsProblem(tn); p != "" {
		return fmt.Errorf("your phone can't connect yet. %s\nThen run `muxscale connect` again", p)
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
	fmt.Printf("\n  Scan with your phone's camera, or in the app: Add machine.\n")
	fmt.Printf("  %s\n\n", base)

	// The code is useless until the phone's identity is permitted, and the
	// failure mode is a 403 on the phone with the fix printed only here. The
	// daemon lets the machine's own login in by itself, a moment after the
	// listener knows who that is.
	var acl struct {
		Allowed []string `json:"allowed"`
	}
	for wait := 0; wait < 8; wait++ {
		if err := apiGet("/v1/acl", &acl); err != nil || len(acl.Allowed) > 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if len(acl.Allowed) == 0 {
		fmt.Println("  Nobody is permitted to connect yet. Allow your Tailscale login:")
		fmt.Println("    muxscale acl allow you@example.com")
	} else {
		fmt.Printf("  Permitted: %s\n", strings.Join(acl.Allowed, ", "))
	}
	fmt.Println()
	return nil
}

// connectOff stops serving to phones. The daemon remembers that too.
func connectOff() error {
	resp, err := apiPost("/v1/tailnet", `{"enable":false}`)
	if err != nil {
		return daemonDown(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		if msg := strings.TrimSpace(string(b)); !strings.Contains(msg, "not running") {
			return fmt.Errorf("%s", msg)
		}
	}
	fmt.Println("No longer serving to your phone. Local access is unaffected.")
	return nil
}

// getTailnet reads the daemon's tailnet listener state.
func getTailnet() (tailnetView, error) {
	var tn tailnetView
	resp, err := apiRequest(http.MethodGet, "/v1/tailnet", nil)
	if err != nil {
		return tn, daemonDown(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return tn, fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	return tn, json.NewDecoder(resp.Body).Decode(&tn)
}

// enableTailnet turns the listener on, on the system node or one of its own.
func enableTailnet(system bool) (tailnetView, error) {
	var tn tailnetView
	body, _ := json.Marshal(map[string]any{"enable": true, "system": system})
	resp, err := apiPost("/v1/tailnet", string(body))
	if err != nil {
		return tn, daemonDown(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return tn, fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	return tn, json.NewDecoder(resp.Body).Decode(&tn)
}

// tailnetBaseURL is where the phone should connect, or why there is nowhere.
func tailnetBaseURL(tn tailnetView) (string, error) {
	if !tn.Enabled {
		return "", fmt.Errorf("the tailnet listener is off, so there is nothing for a phone to reach; " +
			"run `muxscale connect`")
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

// httpsProblem is the daemon's account of why HTTPS will not work, if it has
// one; a URL already on http means the same thing.
func httpsProblem(tn tailnetView) string {
	if tn.Status != nil && tn.Status.HTTPSProblem != "" {
		return tn.Status.HTTPSProblem
	}
	if strings.HasPrefix(tn.URL, "http://") {
		return "this machine is being served without HTTPS, and the muxscale app only " +
			"connects over HTTPS. Turn on HTTPS at https://login.tailscale.com/admin/dns, " +
			"then restart muxscale (`muxscale up`)"
	}
	return ""
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
