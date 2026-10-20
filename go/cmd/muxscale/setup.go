package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/adapter/bob"
	"github.com/muxscale/muxscale/go/internal/adapter/claudecode"
	"github.com/muxscale/muxscale/go/internal/adapter/codex"
	"github.com/muxscale/muxscale/go/internal/adapter/omp"
	"github.com/muxscale/muxscale/go/internal/attach"
	"github.com/muxscale/muxscale/go/internal/protocol"
	"github.com/muxscale/muxscale/go/internal/setup"
)

// ---------- up / down ----------

// cmdUp starts muxscale now and at every login. It is the whole setup: the
// daemon keeps Claude Code's hooks in place itself each time it starts, and
// `muxscale connect` remembers the phone side, so neither is a step here.
//
// Wrapping agents in PATH shims is the one thing left opt-in, since it means
// a line in the user's shell profile.
func cmdUp(args []string) error {
	for _, a := range args {
		if a != "--shims" && a != "--yes" {
			return fmt.Errorf("usage: muxscale up [--shims]")
		}
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	_, alreadyUp := daemonReachable()

	plan, err := setup.BuildServicePlan(bin, false)
	if err != nil {
		return err
	}
	if err := plan.Apply(); err != nil {
		return fmt.Errorf("installing the %s service: %w", plan.Kind, err)
	}
	fmt.Printf("Installed %s, which starts muxscale at login and restarts it if it exits.\n", plan.Path)
	if hint := setup.LingerHint(); hint != "" {
		fmt.Println(hint)
	}

	if hasFlag(args, "--shims") {
		if err := installShims(); err != nil {
			fmt.Printf("Shims: %v\n", err)
		}
	}

	if alreadyUp {
		fmt.Println("\nA daemon was already running outside the service. Stop it, and the")
		fmt.Println("service takes over; sessions in terminals are picked up again.")
		return nil
	}
	for wait := 0; wait < 10; wait++ {
		if _, ok := daemonReachable(); ok {
			fmt.Println("\nmuxscale is running. To use it from your phone:  muxscale connect")
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("\nThe service is installed but the daemon has not answered yet;")
	fmt.Println("`muxscale status` will say why.")
	return nil
}

// installShims writes the PATH shims, for agents started after this.
func installShims() error {
	agents := make([]string, 0, len(attach.AgentBinaries))
	for bin := range attach.AgentBinaries {
		agents = append(agents, bin)
	}
	sort.Strings(agents)
	shims, err := setup.BuildShimPlan(agents)
	if err != nil {
		return err
	}
	if len(shims.Agents) == 0 {
		return fmt.Errorf("no supported agent on PATH to wrap")
	}
	if err := shims.Apply(); err != nil {
		return err
	}
	found := make([]string, 0, len(shims.Agents))
	for name := range shims.Agents {
		found = append(found, name)
	}
	sort.Strings(found)
	fmt.Printf("Wrapped %s in %s, so sessions you start accept replies.\n",
		strings.Join(found, ", "), shims.Dir)
	if !shims.AlreadyOnPath {
		// Never edited without consent: this is the user's shell config.
		fmt.Printf("Add this line to %s, then open a new shell:\n    %s\n", shims.ProfilePath, shims.PathHint)
	}
	return nil
}

// cmdDown removes everything muxscale set up: the service, the hooks and
// their token in Claude Code's settings, and any shims. What it recorded --
// sessions, transcripts, the audit log -- stays, in the state directory.
func cmdDown(args []string) error {
	yes := hasFlag(args, "--yes")

	var removing []string
	svc, _ := setup.ServiceStatus()
	if svc.Installed {
		removing = append(removing, "the "+svc.Kind+" service")
	}
	hooks, err := claudecode.BuildUninstall("")
	if err != nil {
		return err
	}
	if len(hooks.Removing) > 0 {
		removing = append(removing, "muxscale's hooks and token in "+hooks.SettingsPath)
	}
	if omp.ExtensionPresent() {
		removing = append(removing, "the muxscale extension for omp")
	}
	if codex.HooksPresent() {
		removing = append(removing, "muxscale's hooks in ~/.codex/hooks.json")
	}
	shims, _ := setup.InstalledShims()
	if len(shims) > 0 {
		removing = append(removing, "shims for "+strings.Join(shims, ", "))
	}
	if len(removing) == 0 {
		fmt.Println("Nothing of muxscale's is installed.")
		return nil
	}

	fmt.Println("This removes:")
	for _, r := range removing {
		fmt.Println("  " + r)
	}
	if !yes && !confirm("Proceed?") {
		fmt.Println("Nothing changed.")
		return nil
	}

	if svc.Installed {
		if path, err := setup.UninstallService(); err != nil {
			fmt.Printf("service: %v\n", err)
		} else {
			fmt.Printf("removed %s\n", path)
		}
	}
	if len(hooks.Removing) > 0 {
		if err := hooks.Apply(); err != nil {
			fmt.Printf("hooks: %v\n", err)
		} else {
			fmt.Printf("removed the hooks (backup: %s)\n", hooks.BackupPath)
		}
	}
	if omp.ExtensionPresent() {
		if removed, err := omp.RemoveExtension(); err != nil {
			fmt.Printf("omp extension: %v\n", err)
		} else if removed {
			fmt.Println("removed the omp extension")
		}
	}
	if codex.HooksPresent() {
		if removed, err := codex.RemoveHooks(""); err != nil {
			fmt.Printf("codex hooks: %v\n", err)
		} else if removed {
			fmt.Println("removed muxscale's Codex hooks")
		}
	}
	if removed, err := bob.RemoveHooks(); err != nil {
		fmt.Printf("bob hooks: %v\n", err)
	} else if removed {
		fmt.Println("removed muxscale's Bob hooks")
	}
	if len(shims) > 0 {
		if removed, err := setup.UninstallShims(); err != nil {
			fmt.Printf("shims: %v\n", err)
		} else {
			fmt.Printf("removed shims for %s\n", strings.Join(removed, ", "))
		}
	}

	// A daemon started by hand outlives the service, and would put the hooks
	// back the next time it starts.
	if _, ok := daemonReachable(); ok {
		fmt.Println("\nA daemon is still running outside the service; stop it too, or it")
		fmt.Println("sets the hooks up again when it next starts.")
	}
	return nil
}

// daemonReachable reports whether a daemon answers, and its status if so.
func daemonReachable() (map[string]any, bool) {
	var out map[string]any
	if err := apiGet("/v1/status", &out); err != nil {
		return nil, false
	}
	return out, true
}

// ---------- status ----------

// cmdStatus says what is running and reachable, and how to fix what is not.
//
// One view for what used to be five: status, doctor, tailnet, adopt and
// hooks status each answered part of "is this working?", and the part that
// was broken was always in a command nobody ran.
func cmdStatus(args []string) error {
	if hasFlag(args, "--json") {
		var out map[string]any
		if err := apiGet("/v1/status", &out); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}

	broken := false
	line := func(mark, label, detail string) {
		if mark == "FAIL" {
			broken = true
		}
		fmt.Printf("[%s] %-9s %s\n", mark, label, detail)
	}
	fix := func(how string) { fmt.Printf("       %-9s %s\n", "", how) }

	// The daemon, and what it is watching.
	var st struct {
		LocalAddr string `json:"local_addr"`
		Approvals int    `json:"approvals"`
	}
	up := apiGet("/v1/status", &st) == nil
	live := liveSessions()
	if up {
		detail := fmt.Sprintf("%s, %d live session(s)", st.LocalAddr, len(live))
		if st.Approvals > 0 {
			detail += fmt.Sprintf(", %d waiting for approval (`muxscale ls`)", st.Approvals)
		}
		line(" ok ", "daemon", detail)
	} else {
		line("FAIL", "daemon", "not running at "+apiAddr())
		fix("start it with `muxscale up`")
	}

	if svc, err := setup.ServiceStatus(); err == nil {
		switch {
		case svc.Installed && svc.Running:
			line(" ok ", "service", svc.Kind+", starts at login")
		case svc.Installed:
			line("FAIL", "service", svc.Kind+" installed but not running")
			fix("`muxscale up` reinstalls and starts it")
		default:
			line(" -- ", "service", "not installed; muxscale stops when its terminal closes")
			fix("`muxscale up` runs it in the background and at login")
		}
	}

	// Claude Code, and the hooks the daemon keeps in place.
	if v, err := claudecode.DetectVersion(); err != nil {
		line(" -- ", "claude", "not on PATH")
	} else {
		s := claudecode.Classify(v)
		if s == claudecode.SupportNone {
			line("FAIL", "claude", claudecode.SupportMessage(v, s))
		} else {
			line(" ok ", "claude", fmt.Sprintf("%s (%s)", v, s))
			statusHooks(line, fix)
		}
	}

	// omp, and the extension the daemon installs at startup.
	if v, err := omp.DetectVersion(); err != nil {
		line(" -- ", "omp", "not on PATH")
	} else {
		info := omp.New().Info()
		s := info.ClassifyVersion(v)
		if s == adapter.VersionTooOld {
			line("FAIL", "omp", info.VersionMessage(v, s))
		} else {
			line(" ok ", "omp", fmt.Sprintf("%s (%s)", v, s))
			statusOMPExtension(line, fix)
		}
	}

	// Codex, and the hooks that relay its lifecycle into the daemon.
	if v, err := codex.DetectVersion(); err != nil {
		line(" -- ", "codex", "not on PATH")
	} else {
		info := codex.New().Info()
		s := info.ClassifyVersion(v)
		if s == adapter.VersionTooOld {
			line("FAIL", "codex", info.VersionMessage(v, s))
		} else {
			line(" ok ", "codex", fmt.Sprintf("%s (%s)", v, s))
			statusCodexHooks(line, fix)
		}
	}

	// Bob Shell reports state only through its hooks.
	if _, err := exec.LookPath("bob"); err == nil {
		if ok, _ := bob.Installed(context.Background()); ok {
			line(" ok ", "bob hooks", "installed; sessions in trusted workspaces report their state")
		} else {
			line("FAIL", "bob hooks", "not installed")
			fix("restart the daemon; it installs Bob's hooks as it starts")
		}
	}

	// The phone.
	if up {
		statusPhone(line, fix)
		statusReplies(line, fix, live)
	}

	if shims, err := setup.InstalledShims(); err == nil && len(shims) > 0 {
		line(" ok ", "shims", strings.Join(shims, ", "))
	}

	if broken {
		os.Exit(1)
	}
	return nil
}

type statusLine func(mark, label, detail string)

// liveSessions is the IDs of sessions still going: not ended, and not left
// behind by a restart. The rest are history, and counting them made a machine
// with nine agents running read as one with a hundred and forty.
func liveSessions() map[string]bool {
	var out struct {
		Sessions []protocol.Session `json:"sessions"`
	}
	live := map[string]bool{}
	if apiGet("/v1/sessions", &out) != nil {
		return live
	}
	for _, s := range out.Sessions {
		if !s.State.Terminal() && s.State != protocol.StateDetached {
			live[s.ID] = true
		}
	}
	return live
}

// statusHooks checks the hooks and token the daemon writes on every start.
func statusHooks(line statusLine, fix func(string)) {
	path, _ := claudecode.DefaultSettingsPath()
	b, _ := os.ReadFile(path)
	var settings struct {
		Env map[string]string `json:"env"`
	}
	_ = json.Unmarshal(b, &settings)
	token, _ := loadOrCreateToken()
	switch got := settings.Env[claudecode.TokenEnvVar]; {
	case !strings.Contains(string(b), "/v1/hook/claude-code"):
		line("FAIL", "hooks", "not in "+path)
		fix("restart the daemon; it sets them up as it starts")
	case got != token:
		line("FAIL", "hooks", "the token in "+path+" is missing or out of date")
		fix("restart the daemon; it sets them up as it starts")
	default:
		line(" ok ", "hooks", "installed; new sessions report their state exactly")
	}
}

// statusPhone checks the tailnet listener and who may use it.
func statusPhone(line statusLine, fix func(string)) {
	tn, err := getTailnet()
	if err != nil {
		line("FAIL", "phone", err.Error())
		return
	}
	if !tn.Enabled {
		line(" -- ", "phone", "not served to your phone")
		fix("`muxscale connect` serves it and shows the QR code")
		return
	}
	base, err := tailnetBaseURL(tn)
	if err != nil {
		line("FAIL", "phone", err.Error())
		return
	}
	if p := httpsProblem(tn); p != "" {
		line("FAIL", "phone", base+", but "+p)
		return
	}
	var acl struct {
		Allowed []string `json:"allowed"`
	}
	_ = apiGet("/v1/acl", &acl)
	if len(acl.Allowed) == 0 {
		line("FAIL", "phone", base+", but nobody is permitted to connect")
		fix("`muxscale acl allow you@example.com`")
		return
	}
	line(" ok ", "phone", fmt.Sprintf("%s [%s], for %s", base, tn.Mode, strings.Join(acl.Allowed, ", ")))
}

// statusReplies says how many sessions can be replied to, and how to make
// more of them so.
func statusReplies(line statusLine, fix func(string), live map[string]bool) {
	resp, err := apiRequest(http.MethodPost, "/v1/adopt", nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var out struct {
		Sessions []struct {
			ID       string `json:"id"`
			Writable bool   `json:"writable"`
		} `json:"sessions"`
		Terminals []attach.TermCapability `json:"terminals"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return
	}
	writable, total := 0, 0
	for _, s := range out.Sessions {
		if !live[s.ID] {
			continue
		}
		total++
		if s.Writable {
			writable++
		}
	}
	var enabled []string
	for _, t := range out.Terminals {
		if t.Enabled {
			enabled = append(enabled, string(t.Kind))
		}
	}
	detail := fmt.Sprintf("%d of %d live session(s) accept replies", writable, total)
	if len(enabled) > 0 {
		detail += " (through " + strings.Join(enabled, ", ") + ")"
	}
	mark := " ok "
	if writable < total {
		mark = " -- "
	}
	line(mark, "replies", detail)
	for _, t := range out.Terminals {
		if t.Available && !t.Enabled && t.HowToFix != "" {
			fix(string(t.Kind) + ": " + t.HowToFix)
		}
	}
}

// statusOMPExtension checks the file the daemon writes on every start. The
// read path works without it; this says whether state is exact or inferred.
func statusOMPExtension(line statusLine, fix func(string)) {
	token, _ := loadOrCreateToken()
	if omp.ExtensionInstalled("http://"+apiAddr(), token) {
		line(" ok ", "omp extension", "installed; state and turn boundaries are exact")
		return
	}
	line("FAIL", "omp extension", "not installed or out of date")
	fix("restart the daemon; it installs the extension as it starts")
}

// statusCodexHooks checks the command hooks Codex must trust once. Trust is
// recorded by Codex against the handler hash and cannot be asserted from
// here, so "installed" and "trusted" are deliberately separate sentences.
func statusCodexHooks(line statusLine, fix func(string)) {
	exe, err := os.Executable()
	if err != nil {
		exe = "muxscale"
	}
	tokenPath := filepath.Join(stateDirOrEmpty(), "hook-token")
	relay := codex.RelayCommand(exe, apiAddr(), tokenPath)
	if codex.HooksInstalled(relay) {
		line(" ok ", "codex hooks", "installed; trust them once with /hooks in Codex")
		return
	}
	line("FAIL", "codex hooks", "not installed or out of date")
	fix("restart the daemon; then run /hooks in Codex and trust muxscale's hooks")
}
