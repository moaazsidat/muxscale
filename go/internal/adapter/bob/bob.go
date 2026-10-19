// Package bob adapts IBM Bob Shell (`bob`).
//
// Written from Bob Shell 2.0.6's own hooks documentation, which ships inside
// the release (the "Configure Hooks" skill in dist/bob.js) -- not from a
// running install. Bob Shell keeps no conversation file this adapter knows how
// to read, so what it offers is its hooks:
//
//   - Seven events, Claude Code's names: SessionStart, UserPromptSubmit,
//     PreCompact, PostCompact, PreToolUse, PostToolUse, Stop. Global ones live
//     in ~/.bob/settings/settings.json under "hooks", as matcher groups of
//     command or https handlers; global and workspace hooks are combined.
//   - Every payload carries session_id, cwd and hook_event_name, with
//     prompt, tool_name/tool_input/tool_use_id/tool_response and
//     last_assistant_message per event.
//   - Hooks run only in trusted workspaces, invalid configuration is ignored,
//     and a failing handler fails open. There is no approval event: a hook's
//     "allow" does not bypass Bob's own command approval, so approvals stay at
//     the keyboard.
//
// So a Bob session is found by the process scan and its state comes from the
// hooks. Its conversation is not shown: the daemon builds transcripts from an
// agent's own records, and the hooks are not one.
package bob

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// MinVersion is the release whose hook documentation this was written from.
const MinVersion = "2.0.6"

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

var (
	_ adapter.Adapter       = (*Adapter)(nil)
	_ adapter.HookCodec     = (*Adapter)(nil)
	_ adapter.StateInferrer = (*Adapter)(nil)
)

func (*Adapter) Info() adapter.Info {
	return adapter.Info{
		ID:          "bob",
		DisplayName: "IBM Bob",
		MinVersion:  MinVersion,
		Signals: adapter.Signals{
			// Found by the process scan, so only while running.
			Discovery: adapter.SupportDerived,
			// Bob's own Stop and prompt hooks, once installed and the
			// workspace trusted.
			State:      adapter.SupportExact,
			Transcript: adapter.SupportNone,
			Approvals:  adapter.SupportNone,
		},
		Attachments: []protocol.Attachment{
			protocol.AttachWrap, protocol.AttachTmux, protocol.AttachTermctl,
		},
	}
}

// Detect matches the `bob` command and the bobshell package's script.
func (*Adapter) Detect(p adapter.Process) bool {
	if len(p.Argv) == 0 {
		return false
	}
	if filepath.Base(p.Argv[0]) == "bob" {
		return true
	}
	for _, a := range p.Argv[1:] {
		if strings.Contains(a, "bobshell/dist/bob.js") {
			return true
		}
	}
	return false
}

// InferState has nothing to infer from: state arrives through the hooks.
func (*Adapter) InferState(adapter.StateInput) (protocol.StateGuess, bool) {
	return protocol.StateGuess{}, false
}

// ---- hooks ----

// HookPayload is what Bob sends every handler.
type HookPayload struct {
	SessionID            string          `json:"session_id"`
	Cwd                  string          `json:"cwd"`
	HookEventName        string          `json:"hook_event_name"`
	Source               string          `json:"source"`
	Prompt               string          `json:"prompt"`
	Trigger              string          `json:"trigger"`
	ToolName             string          `json:"tool_name"`
	ToolInput            map[string]any  `json:"tool_input"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	LastAssistantMessage *string         `json:"last_assistant_message"`
}

func (*Adapter) HookPath() string { return "/v1/hook/bob" }

func (*Adapter) DecodeHook(body []byte) (adapter.HookEvent, error) {
	var p HookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return adapter.HookEvent{}, err
	}
	if p.SessionID == "" {
		return adapter.HookEvent{}, fmt.Errorf("bob hook payload has no session_id")
	}
	e := adapter.HookEvent{
		AgentID: "bob", SessionID: p.SessionID, Cwd: p.Cwd,
		EventName: p.HookEventName,
		ToolName:  p.ToolName, ToolUseID: p.ToolUseID, ToolInput: p.ToolInput,
	}
	switch p.HookEventName {
	case "SessionStart":
		e.Kind = adapter.HookSessionStart
	case "UserPromptSubmit":
		e.Kind = adapter.HookUserPrompt
	case "PreToolUse":
		e.Kind = adapter.HookToolCall
	case "PostToolUse":
		e.Kind = adapter.HookToolResult
		e.ToolOutput = responseText(p.ToolResponse)
	case "Stop":
		e.Kind = adapter.HookStop
	case "PreCompact", "PostCompact":
		e.Kind = adapter.HookCompaction
	default:
		e.Kind = adapter.HookNotice
	}
	return e, nil
}

// EncodeHookReply is always "no decision": Bob has no approval hook, and an
// empty response is a success that changes nothing.
func (*Adapter) EncodeHookReply(adapter.HookEvent, *protocol.ApprovalDecision) any {
	return map[string]any{}
}

func responseText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// ---- installing ----

// SettingsPath is Bob's global settings file.
func SettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bob", "settings", "settings.json"), nil
}

// hookEvents are the events muxscale wires, all that Bob has.
var hookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "PreCompact", "PostCompact"}

// RelayCommand is the handler muxscale adds: the relay posts the payload to
// the daemon and, with --quiet, prints nothing -- Bob adds a hook's stdout to
// the model's context, and muxscale has nothing to say to the model.
func RelayCommand(exe, addr, tokenFile string) string {
	cmd := fmt.Sprintf("%s hook-relay --agent bob --quiet --addr %s", shellQuote(exe), shellQuote(addr))
	if tokenFile != "" {
		cmd += " --token-file " + shellQuote(tokenFile)
	}
	return cmd
}

// marker identifies muxscale's handlers in a file the user also owns, so
// install and uninstall touch only those.
const marker = "hook-relay --agent bob"

// EnsureHooks merges muxscale's handlers into Bob's global settings,
// preserving everything else, and backs the file up the first time. It
// returns true when the file changed.
func EnsureHooks(relay string) (bool, error) {
	path, err := SettingsPath()
	if err != nil {
		return false, err
	}
	before, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	after, err := withHooks(before, relay)
	if err != nil {
		return false, err
	}
	if bytes.Equal(before, after) {
		return false, nil
	}
	if len(before) > 0 {
		backup := path + ".muxscale.bak"
		if _, err := os.Stat(backup); os.IsNotExist(err) {
			if err := os.WriteFile(backup, before, 0o600); err != nil {
				return false, err
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	tmp := path + ".muxscale-tmp"
	if err := os.WriteFile(tmp, after, 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// withHooks returns settings with muxscale's handlers replacing any it added
// before. A settings file that is not a JSON object is left alone: rewriting
// it would destroy what the user wrote.
func withHooks(settings []byte, relay string) ([]byte, error) {
	doc := map[string]any{}
	if len(bytes.TrimSpace(settings)) > 0 {
		if err := json.Unmarshal(settings, &doc); err != nil {
			return nil, fmt.Errorf("%s is not a JSON object; not touching it: %w", "Bob settings", err)
		}
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for event, groups := range hooks {
		hooks[event] = withoutOurs(groups)
		if list, _ := hooks[event].([]any); len(list) == 0 {
			delete(hooks, event) // Bob rejects an empty group list
		}
	}
	for _, event := range hookEvents {
		list, _ := hooks[event].([]any)
		hooks[event] = append(list, map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": relay, "timeout": 10}},
		})
	}
	doc["hooks"] = hooks
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// withoutOurs drops muxscale's handlers from an event's matcher groups, and
// any group left with none.
func withoutOurs(groups any) any {
	list, ok := groups.([]any)
	if !ok {
		return groups
	}
	var kept []any
	for _, g := range list {
		group, ok := g.(map[string]any)
		if !ok {
			kept = append(kept, g)
			continue
		}
		handlers, _ := group["hooks"].([]any)
		var keep []any
		for _, h := range handlers {
			if hm, ok := h.(map[string]any); ok {
				if cmd, _ := hm["command"].(string); strings.Contains(cmd, marker) {
					continue
				}
			}
			keep = append(keep, h)
		}
		if len(keep) == 0 && len(handlers) > 0 {
			continue
		}
		group["hooks"] = keep
		kept = append(kept, group)
	}
	return kept
}

// Installed reports whether muxscale's handlers are in Bob's settings.
func Installed(context.Context) (bool, error) {
	path, err := SettingsPath()
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bytes.Contains(raw, []byte(marker)), nil
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`!*?[]{}()<>|&;#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RemoveHooks takes muxscale's handlers out of Bob's settings, leaving the
// user's own, and drops a "hooks" object muxscale's removal left empty. It
// returns true when the file changed.
func RemoveHooks() (bool, error) {
	path, err := SettingsPath()
	if err != nil {
		return false, err
	}
	before, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !bytes.Contains(before, []byte(marker)) {
		return false, nil
	}
	doc := map[string]any{}
	if err := json.Unmarshal(before, &doc); err != nil {
		return false, err
	}
	if hooks, ok := doc["hooks"].(map[string]any); ok {
		for event, groups := range hooks {
			hooks[event] = withoutOurs(groups)
			if list, _ := hooks[event].([]any); len(list) == 0 {
				delete(hooks, event)
			}
		}
		if len(hooks) == 0 {
			delete(doc, "hooks")
		}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	tmp := path + ".muxscale-tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}
