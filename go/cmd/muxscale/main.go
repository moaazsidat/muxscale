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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/adapter/bob"
	"github.com/muxscale/muxscale/go/internal/adapter/builtin"
	"github.com/muxscale/muxscale/go/internal/adapter/claudecode"
	"github.com/muxscale/muxscale/go/internal/adapter/codex"
	"github.com/muxscale/muxscale/go/internal/adapter/omp"
	"github.com/muxscale/muxscale/go/internal/daemon"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

const defaultAddr = "127.0.0.1:4455"

// version is stamped by the release build (tools/release.sh) with
// -ldflags "-X main.version=..."; a local build says so.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "up":
		err = cmdUp(os.Args[2:])
	case "down":
		err = cmdDown(os.Args[2:])
	case "daemon":
		err = cmdDaemon(os.Args[2:])
	case "status", "doctor":
		err = cmdStatus(os.Args[2:])
	case "connect":
		err = cmdConnect(os.Args[2:])
	case "acl":
		err = cmdACL(os.Args[2:])
	case "ls":
		err = cmdLs(os.Args[2:])
	case "approve":
		err = cmdDecide("allow", os.Args[2:])
	case "deny":
		err = cmdDecide("deny", os.Args[2:])
	case "send":
		err = cmdSend(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "kill":
		err = cmdKill(os.Args[2:])
	case "audit":
		err = cmdAudit(os.Args[2:])
	case "debug":
		err = cmdDebug(os.Args[2:])
	case "hook-relay":
		err = cmdHookRelay(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("muxscale " + version)
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q (see `muxscale help`)", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "muxscale: "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`muxscale - monitor and unblock your coding agents

Set up
  muxscale up                  Start muxscale now and at every login (--shims: also wrap agents)
  muxscale connect             Serve to your phone over Tailscale and show the QR code to scan
  muxscale connect off         Stop serving to your phone
  muxscale status              What is running and reachable, and how to fix what is not
  muxscale down                Stop muxscale and remove everything it set up

Sessions
  muxscale ls [-w]             List sessions and pending approvals (-w: follow events)
  muxscale approve ID          Allow a pending tool call
  muxscale deny ID             Deny one
  muxscale send ID "text"      Reply to a session
  muxscale run -- CMD          Start CMD as a session muxscale owns
  muxscale kill ID             End a session started with run

More
  muxscale acl [allow|deny U]  Who besides you may connect over Tailscale
  muxscale audit               Who did what, when
  muxscale daemon              Run in the foreground (what up runs in the background)
  muxscale version             Which build this is

`)
}

// ---------- daemon ----------

func cmdDaemon(args []string) error {
	addr := defaultAddr
	hold := 120 * time.Second
	var tailnet, useSystemTS, noHooks bool
	var tsHostname, tsAuthKey string
	tsPort := 4443
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--use-system-ts":
			useSystemTS = true
		case "--no-hooks":
			noHooks = true
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
		default:
			// Unknown flags were silently ignored, which meant a typo like
			// --addr for --listen started the daemon on the default port with
			// no indication anything had been disregarded. For a flag whose
			// whole purpose is to change where the daemon binds, quietly using
			// a different value than the one asked for is the wrong failure.
			return fmt.Errorf("unknown option %q for `muxscale daemon`\n"+
				"valid: --listen ADDR, --hold DURATION, --tailnet, --use-system-ts, --no-hooks, "+
				"--tailnet-hostname NAME, --tailnet-port PORT, --authkey KEY",
				args[i])
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

	if !noHooks {
		ensureHooks(addr, token, log)
	}

	d, err := daemon.New(daemon.Config{
		ListenLocal: addr, LocalToken: token, HoldTime: hold, Log: log,
		Adapters: builtin.Registry(builtin.Options{}),
		Tailnet:  tailnet || useSystemTS, TailnetHostname: tsHostname,
		TailnetAuthKey: tsAuthKey, TailnetPort: tsPort,
		UseSystemTS: useSystemTS,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return d.Run(ctx)
}

// ensureHooks keeps Claude Code's hooks pointed at this daemon, with the
// token they need, so running the daemon is the whole setup. Failure is
// logged, not fatal: without hooks the daemon still works from transcripts,
// only less exactly.
func ensureHooks(addr, token string, log *slog.Logger) {
	plan, err := claudecode.EnsureHooks("http://"+addr+"/v1/hook/claude-code", token)
	switch {
	case err != nil:
		log.Warn("Claude Code hooks not set up", "reason", err)
	case plan != nil:
		log.Info("set up Claude Code hooks; sessions started from now on report their state exactly",
			"settings", plan.SettingsPath, "backup", plan.BackupPath)
	}

	// omp discovers extensions from its agent directory, so this is one file
	// and no settings edit. It is what turns a transcript guess into the
	// agent's own account of a finished turn.
	//
	// Nothing is written when omp is not installed: creating ~/.omp for
	// someone who has never run it is a side effect the daemon should not
	// have. Below the verified floor, the read path still works and status
	// explains why state is inferred.
	if v, err := omp.DetectVersion(); err != nil {
		log.Debug("omp not on PATH; skipping its extension")
	} else if info := omp.New().Info(); info.ClassifyVersion(v) == adapter.VersionTooOld {
		log.Warn("omp is below the supported floor; extension not installed",
			"version", v, "floor", info.MinVersion)
	} else if changed, err := omp.EnsureExtension("http://"+addr, token); err != nil {
		log.Warn("omp extension not set up", "reason", err)
	} else if changed {
		path, _ := omp.ExtensionPath()
		log.Info("set up the omp extension; sessions started from now on report their state exactly",
			"extension", path)
	}

	// Codex hooks are commands, so the hook runs `muxscale hook-relay`, which
	// posts the payload here. Codex requires non-managed hooks to be trusted
	// once in /hooks; until then the read path still works.
	exe, err := os.Executable()
	if err != nil {
		exe = "muxscale"
	}
	tokenPath := filepath.Join(stateDirOrEmpty(), "hook-token")
	relay := codex.RelayCommand(exe, addr, tokenPath)
	if v, err := codex.DetectVersion(); err != nil {
		log.Debug("codex not on PATH; skipping its hooks")
	} else if info := codex.New().Info(); info.ClassifyVersion(v) == adapter.VersionTooOld {
		log.Warn("Codex is below the supported floor; hooks not installed",
			"version", v, "floor", info.MinVersion)
	} else if changed, err := codex.EnsureHooks(relay); err != nil {
		log.Warn("Codex hooks not set up", "reason", err)
	} else if changed {
		path, _ := codex.HooksPath()
		log.Info("set up Codex hooks; trust them once with /hooks in Codex",
			"hooks", path)
	}

	// Bob Shell's hooks are its only report of state. Looked up rather than
	// run: `bob` can stop to ask for its licence before printing a version.
	// Its hooks run only in trusted workspaces, and a broken one fails open.
	if _, err := exec.LookPath("bob"); err != nil {
		log.Debug("bob not on PATH; skipping its hooks")
	} else if changed, err := bob.EnsureHooks(bob.RelayCommand(exe, addr, tokenPath)); err != nil {
		log.Warn("Bob hooks not set up", "reason", err)
	} else if changed {
		path, _ := bob.SettingsPath()
		log.Info("set up Bob hooks; sessions in trusted workspaces report their state exactly",
			"settings", path)
	}
}

// stateDirOrEmpty is the daemon's state directory, or "" when it cannot be
// resolved. The relay treats an empty token path as "no credential" and fails
// open, which is the right outcome for a status path that cannot find home.
func stateDirOrEmpty() string {
	d, err := daemon.DefaultStateDir()
	if err != nil {
		return ""
	}
	return d
}

// ---------- ls ----------

func cmdLs(args []string) error {
	if hasFlag(args, "-w") || hasFlag(args, "--watch") {
		return watch()
	}
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
		fmt.Println("No sessions yet. Start an agent and it appears here.")
		return nil
	}
	fmt.Printf("%-17s %-13s %-18s %-28s %s\n", "ID", "AGENT", "STATE", "CWD", "FOR")
	for _, s := range out.Sessions {
		fmt.Printf("%-17s %-13s %-18s %-28s %s\n",
			s.ID, s.AgentID, stateLabel(s.State),
			shorten(s.Cwd, 28), humanDur(time.Since(s.StateSince)))
		if a := s.PendingApproval; a != nil {
			fmt.Printf("    %s needs approval: %s\n", arrow(), a.Subject)
			if len(a.ToolInput) > 0 {
				fmt.Printf("      %s\n", indent(daemon.FormatToolInput(a.ToolInput), "      "))
			}
			if a.Deadline.IsZero() {
				// Asked at the agent's own keyboard: it waits as long as it takes.
				fmt.Printf("      muxscale approve %s   (or deny)\n", a.ID)
			} else {
				fmt.Printf("      muxscale approve %s   (or deny), %s left\n", a.ID, humanDur(time.Until(a.Deadline)))
			}
		}
	}
	return nil
}

// watch follows the daemon's event stream until interrupted.
func watch() error {
	resp, err := apiRequest(http.MethodGet, "/v1/events", nil)
	if err != nil {
		return daemonDown(err)
	}
	defer resp.Body.Close()

	fmt.Println("following muxscale events (ctrl-c to stop)")
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
		fmt.Printf("        answer with: muxscale approve %s  (or deny)\n", c.Approval.ID)
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

// ---------- approve / deny ----------

// cmdDecide answers a pending approval.
func cmdDecide(behavior string, args []string) error {
	verb := map[string]string{"allow": "approve", "deny": "deny"}[behavior]
	if len(args) != 1 {
		return fmt.Errorf("usage: muxscale %s ID  (the IDs are in `muxscale ls`)", verb)
	}
	body, _ := json.Marshal(map[string]string{"id": args[0], "behavior": behavior, "by": "cli"})
	resp, err := apiPost("/v1/approvals/decide", string(body))
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
		return fmt.Errorf("approval %s not found (already answered, or it timed out)", args[0])
	}
	fmt.Printf("%s %s\n", map[string]string{"allow": "approved", "deny": "denied"}[behavior], args[0])
	return nil
}

// ---------- debug ----------

// cmdDebug holds tools for working on muxscale itself, left out of the help.
func cmdDebug(args []string) error {
	if len(args) >= 2 && args[0] == "transcript" {
		return debugTranscript(args[1:])
	}
	if len(args) >= 2 && args[0] == "adapter" {
		return debugAdapter(args[1:])
	}
	return fmt.Errorf("usage: muxscale debug transcript PATH [--json] | debug adapter AGENT [REF]")
}

// debugAdapter runs one adapter's read path against this machine's real
// files: what it discovers, and what it makes of a transcript. It is how an
// adapter is checked against an agent's actual on-disk format, which is the
// only check that counts -- fixtures are written from what we believe the
// format is.
func debugAdapter(args []string) error {
	reg := builtin.Registry(builtin.Options{})
	a, ok := reg.Get(args[0])
	if !ok {
		var ids []string
		for _, x := range reg.All() {
			ids = append(ids, x.Info().ID)
		}
		return fmt.Errorf("no adapter %q (have: %s)", args[0], strings.Join(ids, ", "))
	}
	info := a.Info()
	fmt.Printf("%s (%s)  discovery=%s state=%s transcript=%s approvals=%s\n\n", info.DisplayName, info.ID,
		info.Signals.Discovery, info.Signals.State, info.Signals.Transcript, info.Signals.Approvals)

	ref := ""
	if len(args) >= 2 {
		ref = args[1]
	}
	if d, ok := a.(adapter.Discoverer); ok {
		found, err := d.Discover(context.Background())
		if err != nil {
			return fmt.Errorf("discover: %w", err)
		}
		live := 0
		for _, s := range found {
			if s.Live {
				live++
			}
		}
		fmt.Printf("discovered %d session(s), %d live\n", len(found), live)
		for i, s := range found {
			if i == 10 {
				fmt.Printf("  … %d more\n", len(found)-i)
				break
			}
			mark := " "
			if s.Live {
				mark = "*"
			}
			fmt.Printf("  %s %-38s %-28s %s\n", mark, clipText(s.NaturalKey, 38), clipText(s.Cwd, 28), clipText(firstOf(s.Name, s.Title), 40))
		}
		if ref == "" && len(found) > 0 {
			ref = found[0].Ref
		}
	} else {
		fmt.Println("no discovery: this adapter only finds sessions through the process scan")
	}
	if ref == "" {
		return nil
	}
	tr, ok := a.(adapter.Transcriber)
	if !ok {
		fmt.Println("\nno transcript reader")
		return nil
	}
	stream, err := tr.OpenTranscript(context.Background(), ref)
	if err != nil {
		return fmt.Errorf("open %s: %w", ref, err)
	}
	defer stream.Close()
	var events []protocol.TranscriptEvent
	for range 50 { // a stream may page; stop once it is drained
		batch, err := stream.Poll(context.Background())
		if err != nil {
			return fmt.Errorf("poll %s: %w", ref, err)
		}
		if len(batch) == 0 {
			break
		}
		events = append(events, batch...)
	}
	fmt.Printf("\ntranscript %s\n  %d event(s)\n", ref, len(events))
	counts := map[protocol.EventKind]int{}
	var order []protocol.EventKind
	for _, e := range events {
		if counts[e.Kind] == 0 {
			order = append(order, e.Kind)
		}
		counts[e.Kind]++
	}
	for _, k := range order {
		fmt.Printf("  %-20s %d\n", k, counts[k])
	}
	for i := max(0, len(events)-3); i < len(events); i++ {
		e := events[i]
		fmt.Printf("  last: %-18s %s %s\n", e.Kind, e.ToolName, clipText(strings.ReplaceAll(e.Text, "\n", " "), 60))
	}
	if si, ok := a.(adapter.StateInferrer); ok && len(events) > 0 {
		in := adapter.StateInput{Ref: ref, Recent: events[max(0, len(events)-8):], Idle: time.Since(events[len(events)-1].At), Quiescence: 6 * time.Second}
		if er, ok := stream.(adapter.EventReporter); ok {
			in.Event = er.LastEvent()
		}
		if lv, ok := a.(adapter.Liveness); ok {
			if live, known := lv.Live(ref); known {
				in.Live = &live
			}
		}
		if g, ok := si.InferState(in); ok {
			fmt.Printf("\nstate %s (%.2f): %s [%s]\n", g.State, g.Confidence, g.Reason, g.Evidence)
		} else {
			fmt.Println("\nstate: no opinion")
		}
	}
	return nil
}

func clipText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// debugTranscript parses a Claude Code JSONL transcript and summarises it.
func debugTranscript(args []string) error {
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
	resp, err := apiPost("/v1/sessions/run", string(body))
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
	resp, err := apiPost("/v1/sessions/input", string(body))
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
	resp, err := apiPost("/v1/sessions/kill", string(body))
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

// ---------- acl ----------

// tailnetView is the shape /v1/tailnet answers with.
type tailnetView struct {
	Enabled bool           `json:"enabled"`
	Mode    string         `json:"mode"`
	URL     string         `json:"url"`
	Status  *tailnetStatus `json:"status"`
}

type tailnetStatus struct {
	Hostname     string   `json:"hostname"`
	DNSName      string   `json:"dns_name"`
	TailnetIPs   []string `json:"tailnet_ips"`
	BackendState string   `json:"backend_state"`
	AuthURL      string   `json:"auth_url"`
	Port         int      `json:"port"`
	HTTPSProblem string   `json:"https_problem"`
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
	resp, err := apiPost("/v1/acl", string(payload))
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

// ---------- helpers ----------

// apiURL is where the CLI looks for the daemon.
//
// MUXSCALE_ADDR overrides the default port. The daemon's listen address is a
// startup flag, so without this a second daemon is unreachable from the CLI --
// and testing a change against a second daemon is exactly how you avoid
// restarting the one that is busy watching someone's sessions.
func apiURL(path string) string { return "http://" + apiAddr() + path }

// apiAddr is the daemon address the CLI will talk to.
func apiAddr() string {
	if x := os.Getenv("MUXSCALE_ADDR"); x != "" {
		return x
	}
	return defaultAddr
}

// apiToken reads the daemon's local token.
//
// Read, never created: the CLI is a client, and a client that mints the
// credential it is about to present is not authenticating, it is asserting.
// If the file is missing the request simply goes without one and the daemon's
// 401 explains itself.
var apiToken = sync.OnceValue(func() string {
	dir, err := daemon.DefaultStateDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, tokenFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
})

// apiRequest calls the daemon with the local token attached.
//
// Every control-API call goes through here. Left to individual call sites, the
// one that forgets the header is the one nobody notices until it 401s in
// somebody else's terminal.
func apiRequest(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, apiURL(path), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if t := apiToken(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	return http.DefaultClient.Do(req)
}

func apiPost(path, body string) (*http.Response, error) {
	return apiRequest(http.MethodPost, path, strings.NewReader(body))
}

func apiGet(path string, out any) error {
	resp, err := apiRequest(http.MethodGet, path, nil)
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
	return fmt.Errorf("daemon not reachable at %s (run `muxscale daemon`): %w", apiAddr(), err)
}

// loadOrCreateToken persists a hook token. M0 uses a file at 0600; the OS
// keychain lands in M1 -- the token must never end up in config.toml or the DB.
const tokenFile = "hook-token"

func loadOrCreateToken() (string, error) {
	dir, err := daemon.DefaultStateDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, tokenFile)
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
