package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func opts(path string) InstallOptions {
	return InstallOptions{
		SettingsPath: path,
		Endpoint:     "http://127.0.0.1:4455/v1/hook/claude-code",
		TokenEnvVar:  "MUXSCALE_HOOK_TOKEN",
		HoldSeconds:  600,
		Events:       []string{HookStop, HookUserPromptSubmit},
		DecisionEvts: []string{HookPermissionRequest},
	}
}

func writeSettings(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func loadJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	return m
}

func TestInstallIntoMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	plan, err := BuildInstall(opts(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	root := loadJSON(t, path)
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		t.Fatal("no hooks written")
	}
	for _, e := range []string{HookStop, HookUserPromptSubmit, HookPermissionRequest} {
		if _, ok := hooks[e]; !ok {
			t.Errorf("event %s not registered", e)
		}
	}
}

// The user's settings.json is theirs. Installing must never drop a key or an
// existing hook -- this is the test that stops a support incident where
// somebody's carefully built config vanishes.
func TestInstallPreservesUnrelatedSettingsAndHooks(t *testing.T) {
	path := writeSettings(t, `{
	  "model": "claude-opus-5",
	  "effortLevel": "high",
	  "hooks": {
	    "Stop": [
	      {"matcher": ".*", "hooks": [{"type": "command", "command": "/usr/local/bin/my-notify"}]}
	    ],
	    "PreToolUse": [
	      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/local/bin/audit"}]}
	    ]
	  }
	}`)

	plan, err := BuildInstall(opts(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}

	root := loadJSON(t, path)
	if root["model"] != "claude-opus-5" || root["effortLevel"] != "high" {
		t.Error("unrelated top-level settings were lost")
	}

	hooks := root["hooks"].(map[string]any)

	// The user's own Stop hook must survive alongside ours.
	stop := hooks["Stop"].([]any)
	var foundUser, foundOurs bool
	for _, e := range stop {
		m := e.(map[string]any)
		hs := m["hooks"].([]any)
		for _, h := range hs {
			hm := h.(map[string]any)
			if hm["command"] == "/usr/local/bin/my-notify" {
				foundUser = true
			}
			if hm["//"] == managedTag {
				foundOurs = true
			}
		}
	}
	if !foundUser {
		t.Error("user's existing Stop hook was clobbered")
	}
	if !foundOurs {
		t.Error("muxscale Stop hook not added")
	}

	// An event we never touched must be byte-identical in intent.
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 1 {
		t.Errorf("PreToolUse should still have exactly the user's 1 entry, got %d", len(pre))
	}
}

// Install must be idempotent: running it twice is a normal thing for a user to
// do, and it must not accumulate duplicate entries.
func TestInstallIsIdempotent(t *testing.T) {
	path := writeSettings(t, `{}`)
	for i := 0; i < 3; i++ {
		plan, err := BuildInstall(opts(path))
		if err != nil {
			t.Fatal(err)
		}
		if err := plan.Apply(); err != nil {
			t.Fatal(err)
		}
	}
	hooks := loadJSON(t, path)["hooks"].(map[string]any)
	stop := hooks[HookStop].([]any)
	if len(stop) != 1 {
		t.Errorf("after 3 installs, Stop has %d entries; want 1", len(stop))
	}
}

func TestUninstallRemovesOnlyOurs(t *testing.T) {
	path := writeSettings(t, `{
	  "hooks": {
	    "Stop": [
	      {"matcher": ".*", "hooks": [{"type": "command", "command": "/usr/local/bin/my-notify"}]}
	    ]
	  }
	}`)
	plan, _ := BuildInstall(opts(path))
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}

	up, err := BuildUninstall(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := up.Apply(); err != nil {
		t.Fatal(err)
	}

	hooks, _ := loadJSON(t, path)["hooks"].(map[string]any)
	stop, _ := hooks["Stop"].([]any)
	if len(stop) != 1 {
		t.Fatalf("Stop should retain the user's 1 entry, got %d", len(stop))
	}
	m := stop[0].(map[string]any)
	h := m["hooks"].([]any)[0].(map[string]any)
	if h["command"] != "/usr/local/bin/my-notify" {
		t.Error("uninstall removed the wrong entry")
	}
	// Events that only ever held muxscale entries should be gone entirely.
	if _, ok := hooks[HookPermissionRequest]; ok {
		t.Error("PermissionRequest should have been removed with its only entry")
	}
}

func TestInstallRefusesInvalidJSON(t *testing.T) {
	path := writeSettings(t, `{ this is not json`)
	if _, err := BuildInstall(opts(path)); err == nil {
		t.Fatal("expected refusal to touch a settings.json we cannot parse")
	}
}

func TestBackupWrittenBeforeChange(t *testing.T) {
	original := `{"model":"claude-opus-5"}`
	path := writeSettings(t, original)
	plan, _ := BuildInstall(opts(path))
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if plan.BackupPath == "" {
		t.Fatal("no backup path recorded")
	}
	b, err := os.ReadFile(plan.BackupPath)
	if err != nil {
		t.Fatalf("backup not written: %v", err)
	}
	if string(b) != original {
		t.Errorf("backup content = %q, want the original %q", b, original)
	}
}

// Observational hooks get a short timeout, since Claude Code waits for every
// HTTP hook; decision hooks a long one, since they exist precisely to block.
func TestHookTimeoutShape(t *testing.T) {
	path := writeSettings(t, `{}`)
	plan, _ := BuildInstall(opts(path))
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	hooks := loadJSON(t, path)["hooks"].(map[string]any)

	handler := func(event string) map[string]any {
		e := hooks[event].([]any)[0].(map[string]any)
		return e["hooks"].([]any)[0].(map[string]any)
	}

	// Claude Code's HTTP hooks have no async mode and it strips the key, so
	// writing one would only make every start see a change.
	obs := handler(HookStop)
	if _, ok := obs["async"]; ok {
		t.Error("an http hook was given an async key Claude Code does not support")
	}
	if obs["timeout"] != float64(5) {
		t.Errorf("observational hook timeout = %v, want a short 5", obs["timeout"])
	}

	dec := handler(HookPermissionRequest)
	if dec["async"] == true {
		t.Error("decision hook must not be async; it has to block for the answer")
	}
	if dec["timeout"] != float64(600) {
		t.Errorf("decision hook timeout = %v, want 600", dec["timeout"])
	}
	if dec["statusMessage"] == "" || dec["statusMessage"] == nil {
		t.Error("decision hook needs a statusMessage so the person at the keyboard knows why it paused")
	}

	// The token must be referenced, never inlined.
	hdrs := dec["headers"].(map[string]any)
	if hdrs["Authorization"] != "Bearer $MUXSCALE_HOOK_TOKEN" {
		t.Errorf("Authorization = %v; the token must be an env reference, not a literal", hdrs["Authorization"])
	}
}

// The token goes in settings.json's env block, so the hooks authenticate in
// any session however it was started, and comes out again on uninstall.
func TestInstallWritesTokenAndUninstallRemovesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"env":{"KEEP":"1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	opt := InstallOptions{SettingsPath: path, Endpoint: "http://127.0.0.1:4455/v1/hook/claude-code",
		Token: "secret", Events: ObservationalEvents, DecisionEvts: DecisionEvents}
	plan, err := BuildInstall(opt)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Changes() {
		t.Fatal("a first install reported no changes")
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	env := root["env"].(map[string]any)
	if env[TokenEnvVar] != "secret" || env["KEEP"] != "1" {
		t.Fatalf("env = %v", env)
	}

	// Same again, but formatted differently by hand: nothing to do.
	var pretty map[string]any
	_ = json.Unmarshal(b, &pretty)
	compact, _ := json.Marshal(pretty)
	if err := os.WriteFile(path, compact, 0o600); err != nil {
		t.Fatal(err)
	}
	if plan, _ := BuildInstall(opt); plan.Changes() {
		t.Fatal("an up-to-date file would be rewritten")
	}
	// A new token is a change.
	opt.Token = "rotated"
	if plan, _ := BuildInstall(opt); !plan.Changes() {
		t.Fatal("a new token was not seen as a change")
	}

	un, err := BuildUninstall(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := un.Apply(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), TokenEnvVar) || !strings.Contains(string(b), `"KEEP"`) {
		t.Fatalf("after uninstall: %s", b)
	}
}

// Regression: Claude Code drops the "//" tags when it rewrites settings.json,
// and install, recognising its entries only by tag, added a second set beside
// the untagged first. Every hook fired twice.
func TestInstallRecognisesItsEntriesWithoutTags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	settings := `{"hooks":{"Stop":[
	  {"matcher":".*","hooks":[{"type":"http","url":"http://127.0.0.1:4455/v1/hook/claude-code","async":true}]},
	  {"matcher":".*","hooks":[{"type":"command","command":"say done"}]}
	]}}`
	if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildInstall(InstallOptions{SettingsPath: path,
		Endpoint: "http://127.0.0.1:4455/v1/hook/claude-code", Events: []string{"Stop"}})
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(plan.After), &root); err != nil {
		t.Fatal(err)
	}
	stop := root.Hooks["Stop"]
	if len(stop) != 2 {
		t.Fatalf("Stop has %d entries, want the user's one and muxscale's one: %v", len(stop), stop)
	}
	if !strings.Contains(plan.After, "say done") {
		t.Fatal("the user's own hook was lost")
	}

	// Installed, then stripped of its tags the way Claude Code does: up to
	// date, so the daemon does not rewrite it on every start.
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	stripped, _ := json.Marshal(untagged(mustJSON(t, plan.After)))
	if err := os.WriteFile(path, stripped, 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := BuildInstall(InstallOptions{SettingsPath: path,
		Endpoint: "http://127.0.0.1:4455/v1/hook/claude-code", Events: []string{"Stop"}})
	if err != nil {
		t.Fatal(err)
	}
	if again.Changes() {
		t.Fatal("a file differing only in muxscale's tags would be rewritten")
	}
}

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
