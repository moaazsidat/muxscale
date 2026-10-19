package bob

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muxscale/muxscale/go/internal/adapter"
)

func TestDecodeHook(t *testing.T) {
	a := New()
	cases := map[string]adapter.HookKind{
		`{"session_id":"task-123","cwd":"/w","hook_event_name":"SessionStart","source":"startup"}`:                                                                 adapter.HookSessionStart,
		`{"session_id":"task-123","cwd":"/w","hook_event_name":"UserPromptSubmit","prompt":"fix it"}`:                                                              adapter.HookUserPrompt,
		`{"session_id":"task-123","cwd":"/w","hook_event_name":"PreToolUse","tool_name":"execute_command","tool_input":{"command":"ls"},"tool_use_id":"tool-456"}`: adapter.HookToolCall,
		`{"session_id":"task-123","cwd":"/w","hook_event_name":"PostToolUse","tool_name":"execute_command","tool_use_id":"tool-456","tool_response":"a.txt"}`:      adapter.HookToolResult,
		`{"session_id":"task-123","cwd":"/w","hook_event_name":"Stop","last_assistant_message":null}`:                                                              adapter.HookStop,
		`{"session_id":"task-123","cwd":"/w","hook_event_name":"PostCompact","trigger":"auto","compact_summary":"s"}`:                                              adapter.HookCompaction,
	}
	for body, want := range cases {
		e, err := a.DecodeHook([]byte(body))
		if err != nil || e.Kind != want || e.SessionID != "task-123" || e.Cwd != "/w" || e.AgentID != "bob" {
			t.Errorf("%s -> %+v, %v", body, e, err)
		}
	}
	e, _ := a.DecodeHook([]byte(`{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"x","tool_response":"a.txt"}`))
	if e.ToolOutput != "a.txt" {
		t.Errorf("tool output = %q", e.ToolOutput)
	}
	if _, err := a.DecodeHook([]byte(`{"hook_event_name":"Stop"}`)); err == nil {
		t.Error("accepted a payload with no session")
	}
}

// Installing must keep the user's own hooks and settings, be idempotent, and
// back up what it changed.
func TestEnsureHooksMergesAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, _ := SettingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	user := `{"theme":"dark","hooks":{"PreToolUse":[{"matcher":"^execute_command$","hooks":[{"type":"command","command":"node .bob/hooks/protect.mjs"}]}]}}`
	if err := os.WriteFile(path, []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}

	relay := RelayCommand("/usr/local/bin/muxscale", "127.0.0.1:4455", "/state/hook-token")
	changed, err := EnsureHooks(relay)
	if err != nil || !changed {
		t.Fatalf("first install: changed=%v err=%v", changed, err)
	}
	if again, err := EnsureHooks(relay); err != nil || again {
		t.Fatalf("second install changed the file: %v %v", again, err)
	}
	if ok, _ := Installed(context.Background()); !ok {
		t.Error("Installed reports false after install")
	}
	if b, err := os.ReadFile(path + ".muxscale.bak"); err != nil || string(b) != user {
		t.Errorf("backup = %q, %v", b, err)
	}

	var doc map[string]any
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["theme"] != "dark" {
		t.Error("lost an unrelated setting")
	}
	hooks := doc["hooks"].(map[string]any)
	for _, ev := range hookEvents {
		if !strings.Contains(string(mustJSON(hooks[ev])), "hook-relay --agent bob") {
			t.Errorf("%s has no muxscale handler", ev)
		}
	}
	pre := string(mustJSON(hooks["PreToolUse"]))
	if !strings.Contains(pre, "protect.mjs") {
		t.Error("lost the user's own PreToolUse hook")
	}
	if strings.Count(pre, "hook-relay") != 1 {
		t.Errorf("PreToolUse has %d muxscale handlers", strings.Count(pre, "hook-relay"))
	}
}

func TestEnsureHooksLeavesInvalidSettingsAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, _ := SettingsPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte("{ not json"), 0o600)
	if _, err := EnsureHooks("muxscale hook-relay --agent bob"); err == nil {
		t.Fatal("rewrote a settings file it could not parse")
	}
	if b, _ := os.ReadFile(path); string(b) != "{ not json" {
		t.Error("changed a settings file it could not parse")
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestRemoveHooksRestoresTheUsersFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, _ := SettingsPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte(`{"theme":"dark"}`), 0o600)
	if _, err := EnsureHooks(RelayCommand("muxscale", "127.0.0.1:4455", "")); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveHooks()
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	var doc map[string]any
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["hooks"]; ok || doc["theme"] != "dark" || len(doc) != 1 {
		t.Errorf("after removal: %s", raw)
	}
	if again, _ := RemoveHooks(); again {
		t.Error("a second removal changed the file")
	}
}
