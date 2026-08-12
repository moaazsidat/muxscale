// Command muxscale is the CLI and daemon for muxscale.
//
// M0 scope: Claude Code hooks end to end, in the terminal only. No phone, no
// cloud, no VT emulator. If this is not correct, nothing downstream matters.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter/builtin"
	"github.com/muxscale/muxscale/go/internal/adapter/claudecode"
	"github.com/muxscale/muxscale/go/internal/attach"
	"github.com/muxscale/muxscale/go/internal/daemon"
	"github.com/muxscale/muxscale/go/internal/protocol"
	"github.com/muxscale/muxscale/go/internal/setup"
)

const defaultAddr = "127.0.0.1:4455"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "daemon":
		err = cmdDaemon(os.Args[2:])
	case "ls":
		err = cmdLs(os.Args[2:])
	case "watch":
		err = cmdWatch(os.Args[2:])
	case "approvals":
		err = cmdApprovals(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "send":
		err = cmdSend(os.Args[2:])
	case "kill":
		err = cmdKill(os.Args[2:])
	case "audit":
		err = cmdAudit(os.Args[2:])
	case "up":
		err = cmdUp(os.Args[2:])
	case "down":
		err = cmdDown(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "acl":
		err = cmdACL(os.Args[2:])
	case "hooks":
		err = cmdHooks(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "transcript":
		err = cmdTranscript(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "muxscale: "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`muxscale - monitor and unblock your coding agents

Usage:
  muxscale up [--tailnet]      Set up: service, hooks, shims (each prompted)
  muxscale down                Stop and remove the background service
  muxscale daemon [--tailnet]  Run the daemon in the foreground
  muxscale status              Daemon, tailnet, and session summary
  muxscale acl [allow|deny U]  Tailnet identities permitted to connect
  muxscale run -- CMD          Launch CMD as a managed session (enables replies)
  muxscale send ID "text"      Send input to a session
  muxscale kill ID             Terminate a wrapped session
  muxscale ls                  List sessions
  muxscale watch               Stream session events
  muxscale approvals           List pending approvals
  muxscale approvals allow ID  Answer a pending approval
  muxscale approvals deny ID
  muxscale hooks install       Install Claude Code hooks (shows the diff first)
  muxscale hooks uninstall     Remove exactly muxscale's hook entries
  muxscale hooks status
  muxscale doctor              Diagnose the local setup
  muxscale audit               Recent audit log
  muxscale transcript PATH     Parse a Claude Code JSONL transcript

`)
}

// ---------- daemon ----------

func cmdDaemon(args []string) error {
	addr := defaultAddr
	hold := 120 * time.Second
	var tailnet bool
	var tsHostname, tsAuthKey string
	tsPort := 4443
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tailnet":
			tailnet = true
		case "--tailnet-hostname":
			if i+1 < len(args) {
				i++
				tsHostname = args[i]
			}
		case "--authkey":
			if i+1 < len(args) {
				i++
				tsAuthKey = args[i]
			}
		case "--tailnet-port":
			if i+1 < len(args) {
				i++
				p, err := strconv.Atoi(args[i])
				if err != nil {
					return fmt.Errorf("bad --tailnet-port: %w", err)
				}
				tsPort = p
			}
		case "--listen":
			if i+1 < len(args) {
				i++
				addr = args[i]
			}
		case "--hold":
			if i+1 < len(args) {
				i++
				d, err := time.ParseDuration(args[i])
				if err != nil {
					return fmt.Errorf("bad --hold: %w", err)
				}
				hold = d
			}
		}
	}

	token, err := loadOrCreateToken()
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// An auth key on the command line lands in shell history and `ps` output.
	// Prefer the environment, and say so rather than silently accepting it.
	if tsAuthKey == "" {
		tsAuthKey = os.Getenv("MUXSCALE_TS_AUTHKEY")
	} else {
		fmt.Fprintln(os.Stderr,
			"warning: --authkey is visible in shell history and `ps`; "+
				"prefer MUXSCALE_TS_AUTHKEY in the environment")
	}

	d, err := daemon.New(daemon.Config{
		ListenLocal: addr, HookToken: token, HoldTime: hold, Log: log,
		Adapters: builtin.Registry(builtin.Options{}),
		Tailnet:  tailnet, TailnetHostname: tsHostname,
		TailnetAuthKey: tsAuthKey, TailnetPort: tsPort,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return d.Run(ctx)
}

// ---------- ls ----------

func cmdLs(args []string) error {
	asJSON := hasFlag(args, "--json")
	var out struct {
		Sessions []protocol.Session `json:"sessions"`
	}
	if err := apiGet("/v1/sessions", &out); err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(out.Sessions)
	}
	if len(out.Sessions) == 0 {
		fmt.Println("No sessions yet.")
		fmt.Println("Start a Claude Code session, or run `muxscale hooks install` if you haven't.")
		return nil
	}
	fmt.Printf("%-17s %-13s %-18s %-28s %s\n", "ID", "AGENT", "STATE", "CWD", "FOR")
	for _, s := range out.Sessions {
		fmt.Printf("%-17s %-13s %-18s %-28s %s\n",
			s.ID, s.AgentID, stateLabel(s.State),
			shorten(s.Cwd, 28), humanDur(time.Since(s.StateSince)))
		if s.PendingApproval != nil {
			fmt.Printf("    %s needs approval: %s\n", arrow(), s.PendingApproval.Subject)
		}
	}
	return nil
}

// ---------- watch ----------

func cmdWatch(args []string) error {
	resp, err := http.Get(apiURL("/v1/events"))
	if err != nil {
		return daemonDown(err)
	}
	defer resp.Body.Close()

	fmt.Println("watching muxscale events (ctrl-c to stop)")
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var c daemon.Change
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &c); err != nil {
			continue
		}
		printChange(c)
	}
	return sc.Err()
}

func printChange(c daemon.Change) {
	ts := c.At.Format("15:04:05")
	switch c.Kind {
	case daemon.ChangeStateChange:
		fmt.Printf("%s  %s  %s -> %s  (%s)\n", ts, c.Session.ID,
			c.Prev, stateLabel(c.Session.State), c.Session.Guess.Reason)
	case daemon.ChangeApprovalAsk:
		fmt.Printf("%s  %s  APPROVAL %s\n", ts, c.Approval.SessionID, c.Approval.Subject)
		fmt.Printf("        answer with: muxscale approvals allow %s\n", c.Approval.ID)
	case daemon.ChangeApprovalResolve:
		fmt.Printf("%s  %s  approval resolved\n", ts, c.Approval.SessionID)
	case daemon.ChangeTranscript:
		e := c.Event
		switch e.Kind {
		case protocol.EventToolCall:
			fmt.Printf("%s  %s  tool %s\n", ts, e.SessionID, e.ToolName)
		case protocol.EventToolResult:
			status := "ok"
			if e.IsError {
				status = "error"
			}
			fmt.Printf("%s  %s  tool %s -> %s\n", ts, e.SessionID, e.ToolName, status)
		}
	}
}

// ---------- approvals ----------

func cmdApprovals(args []string) error {
	if len(args) >= 2 && (args[0] == "allow" || args[0] == "deny") {
		body, _ := json.Marshal(map[string]string{
			"id": args[1], "behavior": args[0], "by": "cli",
		})
		resp, err := http.Post(apiURL("/v1/approvals/decide"), "application/json", strings.NewReader(string(body)))
		if err != nil {
			return daemonDown(err)
		}
		defer resp.Body.Close()
		var out struct {
			Delivered bool `json:"delivered"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return err
		}
		if !out.Delivered {
			return fmt.Errorf("approval %s not found (already answered, or it timed out)", args[1])
		}
		fmt.Printf("%s %s\n", args[0], args[1])
		return nil
	}

	var out struct {
		Approvals []protocol.ApprovalRequest `json:"approvals"`
	}
	if err := apiGet("/v1/approvals", &out); err != nil {
		return err
	}
	if len(out.Approvals) == 0 {
		fmt.Println("No pending approvals.")
		return nil
	}
	for _, a := range out.Approvals {
		fmt.Printf("%s  %s\n", a.ID, a.Subject)
		fmt.Printf("    session %s  asked %s ago  expires in %s\n",
			a.SessionID, humanDur(time.Since(a.AskedAt)), humanDur(time.Until(a.Deadline)))
		if len(a.ToolInput) > 0 {
			fmt.Printf("    %s\n", indent(daemon.FormatToolInput(a.ToolInput), "    "))
		}
	}
	return nil
}

// ---------- run / send / kill ----------

func cmdRun(args []string) error {
	// Everything after `--` is the command; anything before is ours.
	var name, dir string
	var cmd []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--":
			cmd = args[i+1:]
			i = len(args)
		case "--name":
			if i+1 < len(args) {
				i++
				name = args[i]
			}
		case "--dir":
			if i+1 < len(args) {
				i++
				dir = args[i]
			}
		}
	}
	if len(cmd) == 0 {
		return fmt.Errorf("usage: muxscale run [--name N] [--dir D] -- COMMAND [ARGS...]")
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}

	body, _ := json.Marshal(map[string]any{"command": cmd, "dir": dir, "name": name})
	resp, err := http.Post(apiURL("/v1/sessions/run"), "application/json", strings.NewReader(string(body)))
	if err != nil {
		return daemonDown(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	var sess protocol.Session
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		return err
	}
	fmt.Printf("started %s (%s) in %s\n", sess.ID, sess.AgentID, sess.Cwd)
	fmt.Printf("send input with: muxscale send %s \"your message\"\n", sess.ID)
	return nil
}

func cmdSend(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf(`usage: muxscale send SESSION_ID "text" [--key enter] [--no-submit] [--fence-prompt ID]`)
	}
	sessionID := args[0]
	var text, key, fencePrompt, fenceState string
	submit := true
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--key":
			if i+1 < len(args) {
				i++
				key = args[i]
			}
		case "--no-submit":
			submit = false
		case "--fence-prompt":
			if i+1 < len(args) {
				i++
				fencePrompt = args[i]
			}
		case "--fence-state":
			if i+1 < len(args) {
				i++
				fenceState = args[i]
			}
		default:
			if !strings.HasPrefix(args[i], "--") && text == "" {
				text = args[i]
			}
		}
	}
	if text == "" && key == "" {
		return fmt.Errorf("nothing to send: give text or --key")
	}

	body, _ := json.Marshal(map[string]any{
		"session_id": sessionID, "text": text, "key": key, "submit": submit,
		"fence": map[string]string{"prompt_id": fencePrompt, "state": fenceState},
		"by":    "cli",
	})
	resp, err := http.Post(apiURL("/v1/sessions/input"), "application/json", strings.NewReader(string(body)))
	if err != nil {
		return daemonDown(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		var e struct {
			Detail string `json:"detail"`
			Want   string `json:"want"`
			Got    string `json:"got"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		// A stale fence is not an error to shout about; it is the safety
		// mechanism doing its job. Explain and offer the override.
		fmt.Fprintf(os.Stderr, "Not sent: the session moved on.\n  %s\n", e.Detail)
		fmt.Fprintf(os.Stderr, "\nRe-check the session, then resend without --fence-prompt to force.\n")
		os.Exit(3)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	fmt.Println("sent")
	return nil
}

func cmdKill(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: muxscale kill SESSION_ID")
	}
	body, _ := json.Marshal(map[string]string{"session_id": args[0]})
	resp, err := http.Post(apiURL("/v1/sessions/kill"), "application/json", strings.NewReader(string(body)))
	if err != nil {
		return daemonDown(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	fmt.Printf("killed %s\n", args[0])
	return nil
}

func cmdAudit(args []string) error {
	var out struct {
		Entries []struct {
			At        time.Time `json:"at"`
			Actor     string    `json:"actor"`
			Action    string    `json:"action"`
			SessionID string    `json:"session_id"`
			Detail    string    `json:"detail"`
		} `json:"entries"`
	}
	if err := apiGet("/v1/audit?limit=50", &out); err != nil {
		return err
	}
	if len(out.Entries) == 0 {
		fmt.Println("No audit entries.")
		return nil
	}
	for _, e := range out.Entries {
		fmt.Printf("%s  %-8s %-26s %-17s %s\n",
			e.At.Format("01-02 15:04:05"), e.Actor, e.Action, e.SessionID, e.Detail)
	}
	return nil
}

// ---------- up / down ----------

// cmdUp is the one command a new user runs. Every step that changes their
// machine is shown first and asked about individually, so nobody ends up with
// a modified shell profile they did not agree to.
func cmdUp(args []string) error {
	yes := hasFlag(args, "--yes")
	tailnet := hasFlag(args, "--tailnet")

	fmt.Println("Setting up muxscale.")
	fmt.Println("Each step is optional and shown before it happens.")
	fmt.Println()

	// 1. Background service.
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	plan, err := setup.BuildServicePlan(bin, tailnet)
	if err != nil {
		fmt.Printf("  service    skipped: %v\n", err)
	} else {
		fmt.Printf("1. Background service (%s)\n", plan.Kind)
		fmt.Printf("   Writes %s\n", plan.Path)
		fmt.Printf("   Starts muxscale at login and restarts it if it exits.\n")
		if plan.Exists {
			fmt.Printf("   (a unit is already installed; it will be replaced)\n")
		}
		if yes || confirm("   Install it?") {
			if err := plan.Apply(); err != nil {
				fmt.Printf("   failed: %v\n", err)
			} else {
				fmt.Printf("   installed.\n")
				if hint := setup.LingerHint(); hint != "" {
					fmt.Printf("   %s\n", hint)
				}
			}
		}
		fmt.Println()
	}

	// 2. Claude Code hooks.
	fmt.Println("2. Claude Code hooks")
	if v, err := claudecode.DetectVersion(); err != nil {
		fmt.Printf("   skipped: %v\n", err)
	} else if claudecode.Classify(v) == claudecode.SupportNone {
		fmt.Printf("   %s\n", claudecode.SupportMessage(v, claudecode.SupportNone))
	} else {
		fmt.Printf("   Claude Code %s detected.\n", v)
		fmt.Printf("   Hooks give exact blocked-state detection and remote approvals.\n")
		fmt.Printf("   Existing hooks are preserved and a backup is written.\n")
		if yes || confirm("   Install them?") {
			if err := hooksInstall(true); err != nil {
				fmt.Printf("   failed: %v\n", err)
			}
		}
	}
	fmt.Println()

	// 3. PATH shims.
	agents := make([]string, 0, len(attach.AgentBinaries))
	for bin := range attach.AgentBinaries {
		agents = append(agents, bin)
	}
	sort.Strings(agents)

	shims, err := setup.BuildShimPlan(agents)
	if err != nil {
		return err
	}
	fmt.Println("3. PATH shims")
	if len(shims.Agents) == 0 {
		fmt.Println("   No supported agent binaries found on PATH; nothing to shim.")
	} else {
		found := make([]string, 0, len(shims.Agents))
		for name := range shims.Agents {
			found = append(found, name)
		}
		sort.Strings(found)
		fmt.Printf("   Found: %s\n", strings.Join(found, ", "))
		fmt.Printf("   Writes wrappers into %s so you keep typing `claude` as usual,\n", shims.Dir)
		fmt.Printf("   but sessions become writable from your phone. Scripts and CI are untouched.\n")
		if yes || confirm("   Install shims?") {
			if err := shims.Apply(); err != nil {
				fmt.Printf("   failed: %v\n", err)
			} else {
				fmt.Printf("   installed.\n")
				if !shims.AlreadyOnPath {
					// Never edited without consent: this is the user's shell config.
					fmt.Printf("\n   Add this line to %s, then restart your shell:\n", shims.ProfilePath)
					fmt.Printf("       %s\n", shims.PathHint)
				}
			}
		}
	}
	fmt.Println()

	// 4. Terminal remote control, for adopting sessions already running.
	fmt.Println("4. Terminal remote control (optional)")
	fmt.Println("   Lets muxscale reply to sessions that are already running,")
	fmt.Println("   without restarting them.")
	for _, c := range attach.DetectTerminals() {
		switch {
		case c.Enabled:
			fmt.Printf("   %-8s enabled\n", c.Kind)
		case c.Available:
			fmt.Printf("   %-8s %s\n", c.Kind, c.Reason)
			if c.HowToFix != "" {
				fmt.Printf("            fix: %s\n", c.HowToFix)
			}
		}
	}
	fmt.Println()

	fmt.Println("Done. Next:")
	fmt.Println("    muxscale status     see the daemon and tailnet")
	fmt.Println("    muxscale ls         see your agents")
	if tailnet {
		fmt.Println("    muxscale acl allow you@example.com   permit your phone")
	}
	return nil
}

func cmdDown(args []string) error {
	path, err := setup.UninstallService()
	if err != nil {
		return err
	}
	fmt.Printf("removed %s\n", path)
	fmt.Println("Hooks and shims are untouched; remove them with")
	fmt.Println("    muxscale hooks uninstall")
	return nil
}

// ---------- status / acl ----------

func cmdStatus(args []string) error {
	var out struct {
		LocalAddr string   `json:"local_addr"`
		Hold      string   `json:"hold"`
		Sessions  int      `json:"sessions"`
		Approvals int      `json:"approvals"`
		DB        string   `json:"db"`
		ACL       []string `json:"acl"`
		Tailnet   *struct {
			Hostname     string   `json:"hostname"`
			DNSName      string   `json:"dns_name"`
			TailnetIPs   []string `json:"tailnet_ips"`
			BackendState string   `json:"backend_state"`
			AuthURL      string   `json:"auth_url"`
			Port         int      `json:"port"`
		} `json:"tailnet"`
	}
	if err := apiGet("/v1/status", &out); err != nil {
		return err
	}
	if hasFlag(args, "--json") {
		return json.NewEncoder(os.Stdout).Encode(out)
	}

	fmt.Printf("daemon      %s  (approval hold %s)\n", out.LocalAddr, out.Hold)
	fmt.Printf("sessions    %d  (%d pending approval)\n", out.Sessions, out.Approvals)
	fmt.Printf("database    %s\n", out.DB)

	if out.Tailnet == nil {
		fmt.Printf("tailnet     not enabled  (start the daemon with --tailnet)\n")
		return nil
	}
	t := out.Tailnet
	fmt.Printf("tailnet     %s  [%s]\n", t.Hostname, t.BackendState)
	if t.AuthURL != "" {
		// This is the whole reason someone runs `status` on first launch.
		fmt.Printf("\n  Log in to finish connecting:\n    %s\n\n", t.AuthURL)
		return nil
	}
	if t.DNSName != "" {
		fmt.Printf("            https://%s:%d\n", t.DNSName, t.Port)
	}
	for _, ip := range t.TailnetIPs {
		fmt.Printf("            %s\n", ip)
	}
	if len(out.ACL) > 0 {
		fmt.Printf("permitted   %s\n", strings.Join(out.ACL, ", "))
	} else {
		fmt.Printf("permitted   nobody yet - run `muxscale acl allow you@example.com`\n")
	}
	return nil
}

func cmdACL(args []string) error {
	if len(args) == 0 {
		var out struct {
			Allowed []string `json:"allowed"`
		}
		if err := apiGet("/v1/acl", &out); err != nil {
			return err
		}
		if len(out.Allowed) == 0 {
			fmt.Println("Nobody is permitted to reach this daemon over the tailnet.")
			fmt.Println("Add yourself with: muxscale acl allow you@example.com")
			return nil
		}
		for _, a := range out.Allowed {
			fmt.Println(a)
		}
		return nil
	}
	if len(args) < 2 {
		return fmt.Errorf("usage: muxscale acl [allow|deny|owner] IDENTITY")
	}
	verb, who := args[0], args[1]
	body := map[string]string{}
	switch verb {
	case "allow", "deny", "owner":
		body[verb] = who
	default:
		return fmt.Errorf("unknown: acl %s (want allow, deny, or owner)", verb)
	}
	payload, _ := json.Marshal(body)
	resp, err := http.Post(apiURL("/v1/acl"), "application/json", strings.NewReader(string(payload)))
	if err != nil {
		return daemonDown(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	var out struct {
		Allowed []string `json:"allowed"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	fmt.Printf("%s %s\n", verb, who)
	for _, a := range out.Allowed {
		fmt.Println("  " + a)
	}
	return nil
}

// ---------- hooks ----------

func cmdHooks(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: muxscale hooks install|uninstall|status")
	}
	switch args[0] {
	case "install":
		return hooksInstall(hasFlag(args, "--yes"))
	case "uninstall":
		return hooksUninstall(hasFlag(args, "--yes"))
	case "status":
		return hooksStatus()
	}
	return fmt.Errorf("unknown: hooks %s", args[0])
}

func hooksInstall(yes bool) error {
	v, err := claudecode.DetectVersion()
	if err != nil {
		return fmt.Errorf("could not detect Claude Code (%w).\nIs `claude` on your PATH?", err)
	}
	support := claudecode.Classify(v)
	fmt.Println(claudecode.SupportMessage(v, support))
	if support == claudecode.SupportNone {
		// Refuse rather than install a weaker safety model.
		return fmt.Errorf("refusing to install hooks below the supported floor")
	}

	token, err := loadOrCreateToken()
	if err != nil {
		return err
	}
	plan, err := claudecode.BuildInstall(claudecode.InstallOptions{
		Endpoint:     "http://" + defaultAddr + "/v1/hook/claude-code",
		TokenEnvVar:  "MUXSCALE_HOOK_TOKEN",
		HoldSeconds:  600,
		Events:       claudecode.ObservationalEvents,
		DecisionEvts: claudecode.DecisionEvents,
	})
	if err != nil {
		return err
	}

	fmt.Printf("\nThis will modify %s\n", plan.SettingsPath)
	fmt.Printf("Registering %d observational hooks (async) and %d decision hooks.\n",
		len(claudecode.ObservationalEvents), len(claudecode.DecisionEvents))
	fmt.Println("Existing hooks are preserved; a timestamped backup is written first.")
	if !yes && !confirm("\nProceed?") {
		fmt.Println("Nothing changed.")
		return nil
	}
	if err := plan.Apply(); err != nil {
		return err
	}
	fmt.Printf("Installed. Backup: %s\n", plan.BackupPath)
	fmt.Printf("\nAdd this to your shell profile so Claude Code can authenticate to the daemon:\n")
	fmt.Printf("    export MUXSCALE_HOOK_TOKEN=%s\n", token)
	fmt.Printf("\nThen start the daemon (`muxscale daemon`) and a new Claude Code session.\n")
	return nil
}

func hooksUninstall(yes bool) error {
	plan, err := claudecode.BuildUninstall("")
	if err != nil {
		return err
	}
	if len(plan.Removing) == 0 {
		fmt.Println("No muxscale hooks installed.")
		return nil
	}
	fmt.Printf("Removing muxscale entries from %s (%d events)\n", plan.SettingsPath, len(plan.Removing))
	if !yes && !confirm("Proceed?") {
		fmt.Println("Nothing changed.")
		return nil
	}
	if err := plan.Apply(); err != nil {
		return err
	}
	fmt.Printf("Removed. Backup: %s\n", plan.BackupPath)
	return nil
}

func hooksStatus() error {
	path, err := claudecode.DefaultSettingsPath()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		fmt.Printf("%s does not exist; no hooks installed.\n", path)
		return nil
	}
	if err != nil {
		return err
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	hooks, _ := root["hooks"].(map[string]any)
	if len(hooks) == 0 {
		fmt.Println("No hooks configured.")
		return nil
	}
	fmt.Printf("%s\n", path)
	for event := range hooks {
		fmt.Printf("  %s\n", event)
	}
	return nil
}

// ---------- doctor ----------

func cmdDoctor(args []string) error {
	ok := true
	check := func(label string, good bool, detail string) {
		mark := "FAIL"
		if good {
			mark = " ok "
		} else {
			ok = false
		}
		fmt.Printf("[%s] %-28s %s\n", mark, label, detail)
	}

	v, err := claudecode.DetectVersion()
	if err != nil {
		check("claude on PATH", false, err.Error())
	} else {
		s := claudecode.Classify(v)
		check("claude version", s != claudecode.SupportNone,
			fmt.Sprintf("%s (%s, floor %s)", v, s, claudecode.MinVersion))
	}

	var sessions struct {
		Sessions []protocol.Session `json:"sessions"`
	}
	if err := apiGet("/v1/sessions", &sessions); err != nil {
		check("daemon", false, "not reachable at "+defaultAddr+" - run `muxscale daemon`")
	} else {
		check("daemon", true, fmt.Sprintf("%s, %d session(s)", defaultAddr, len(sessions.Sessions)))
	}

	if os.Getenv("MUXSCALE_HOOK_TOKEN") == "" {
		check("MUXSCALE_HOOK_TOKEN", false, "not set in this shell; hooks will be rejected")
	} else {
		check("MUXSCALE_HOOK_TOKEN", true, "set")
	}

	path, _ := claudecode.DefaultSettingsPath()
	if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), "muxscale-managed") {
		check("hooks installed", true, path)
	} else {
		check("hooks installed", false, "run `muxscale hooks install`")
	}

	if svc, err := setup.ServiceStatus(); err == nil {
		detail := "not installed - run `muxscale up`"
		if svc.Installed && svc.Running {
			detail = svc.Kind + ", running"
		} else if svc.Installed {
			detail = svc.Kind + ", installed but not running"
		}
		check("background service", svc.Installed && svc.Running, detail)
	}

	if shims, err := setup.InstalledShims(); err == nil {
		if len(shims) == 0 {
			check("path shims", false, "none - sessions you start will be read-only")
		} else {
			check("path shims", true, strings.Join(shims, ", "))
		}
	}

	// Terminal remote control is the difference between adopting an
	// already-running session and being able only to watch it.
	var anyTerm bool
	for _, c := range attach.DetectTerminals() {
		if c.Enabled {
			anyTerm = true
			check("terminal control", true, string(c.Kind)+" remote control enabled")
		}
	}
	if !anyTerm {
		fmt.Printf("[ -- ] %-28s %s\n", "terminal control",
			"no terminal remote control; already-running sessions are read-only")
		for _, c := range attach.DetectTerminals() {
			if c.Available && c.HowToFix != "" {
				fmt.Printf("       %s: %s\n", c.Kind, c.HowToFix)
			}
		}
	}

	if !ok {
		os.Exit(1)
	}
	return nil
}

// ---------- transcript ----------

func cmdTranscript(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: muxscale transcript PATH [--json]")
	}
	events, meta, err := claudecode.ParseTranscriptFile(args[0])
	if err != nil {
		return err
	}
	if hasFlag(args, "--json") {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"meta": meta, "events": events})
	}
	fmt.Printf("session %s\n", meta.SessionID)
	if meta.Title != "" {
		fmt.Printf("title   %s\n", meta.Title)
	}
	fmt.Printf("cwd     %s\n", meta.Cwd)
	if meta.GitBranch != "" {
		fmt.Printf("branch  %s\n", meta.GitBranch)
	}
	fmt.Printf("model   %s\nversion %s\nevents  %d\n\n", meta.Model, meta.Version, len(events))

	counts := map[protocol.EventKind]int{}
	for _, e := range events {
		counts[e.Kind]++
	}
	for _, k := range []protocol.EventKind{
		protocol.EventUserMessage, protocol.EventAssistantMessage, protocol.EventThinking,
		protocol.EventToolCall, protocol.EventToolResult,
	} {
		if counts[k] > 0 {
			fmt.Printf("  %-20s %d\n", k, counts[k])
		}
	}
	return nil
}

// ---------- helpers ----------

func apiURL(path string) string { return "http://" + defaultAddr + path }

func apiGet(path string, out any) error {
	resp, err := http.Get(apiURL(path))
	if err != nil {
		return daemonDown(err)
	}
	defer resp.Body.Close()
	// Error responses are plain text, so decoding them as JSON would surface a
	// parser error instead of the message the daemon actually wrote.
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("%s", msg)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func daemonDown(err error) error {
	return fmt.Errorf("daemon not reachable at %s (run `muxscale daemon`): %w", defaultAddr, err)
}

// loadOrCreateToken persists a hook token. M0 uses a file at 0600; the OS
// keychain lands in M1 -- the token must never end up in config.toml or the DB.
func loadOrCreateToken() (string, error) {
	dir, err := daemon.DefaultStateDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "hook-token")
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		return strings.TrimSpace(string(b)), nil
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw[:])
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func hasFlag(args []string, f string) bool {
	for _, a := range args {
		if a == f {
			return true
		}
	}
	return false
}

func confirm(prompt string) bool {
	fmt.Print(prompt + " [y/N] ")
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return false
	}
	a := strings.ToLower(strings.TrimSpace(sc.Text()))
	return a == "y" || a == "yes"
}

func stateLabel(s protocol.SessionState) string {
	if s.Waiting() {
		return "* " + string(s)
	}
	return "  " + string(s)
}

func arrow() string { return "->" }

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n+1:]
}

func indent(s, pad string) string {
	return strings.ReplaceAll(s, "\n", "\n"+pad)
}

func humanDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
