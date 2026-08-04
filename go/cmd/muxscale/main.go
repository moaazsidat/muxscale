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
	"strings"
	"syscall"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter/claudecode"
	"github.com/muxscale/muxscale/go/internal/daemon"
	"github.com/muxscale/muxscale/go/internal/protocol"
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
  muxscale daemon              Run the daemon (hook listener + session registry)
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
	for i := 0; i < len(args); i++ {
		switch args[i] {
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
	d, err := daemon.New(daemon.Config{
		ListenLocal: addr, HookToken: token, HoldTime: hold, Log: log,
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
