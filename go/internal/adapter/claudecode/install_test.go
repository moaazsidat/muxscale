package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// Observational hooks must be async so they never sit in the agent's critical
// path; decision hooks must not be, since they exist precisely to block.
func TestHookAsyncAndTimeoutShape(t *testing.T) {
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

	obs := handler(HookStop)
	if obs["async"] != true {
		t.Error("observational hook must be async")
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
