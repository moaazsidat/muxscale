package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const relay = "'/usr/local/bin/muxscale' hook-relay --agent codex --addr '127.0.0.1:4455' --token-file '/tmp/token'"

func readDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestEnsureHooksPreservesUserHooks(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	path, _ := HooksPath()
	if err := os.WriteFile(path, []byte(`{
	  "description": "mine",
	  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "echo user"}]}]},
	  "other": true
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureHooks(relay)
	if err != nil || !changed {
		t.Fatalf("install = %v, %v", changed, err)
	}
	if !HooksInstalled(relay) {
		t.Fatal("hooks not recognised as installed")
	}
	doc := readDoc(t, path)
	if doc["description"] != "mine" || doc["other"] != true {
		t.Errorf("top-level user data lost: %+v", doc)
	}
	hooks := doc["hooks"].(map[string]any)
	stop := hooks["Stop"].([]any)
	if len(stop) != 2 {
		t.Fatalf("Stop groups = %d, want user + muxscale", len(stop))
	}
	if _, ok := hooks["PermissionRequest"]; !ok {
		t.Error("PermissionRequest not wired")
	}

	changed, err = EnsureHooks(relay)
	if err != nil || changed {
		t.Fatalf("second install = %v, %v", changed, err)
	}

	removed, err := RemoveHooks("")
	if err != nil || !removed {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	doc = readDoc(t, path)
	hooks = doc["hooks"].(map[string]any)
	if _, ok := hooks["PermissionRequest"]; ok {
		t.Error("muxscale handlers survived uninstall")
	}
	if len(hooks["Stop"].([]any)) != 1 {
		t.Errorf("user Stop hook lost: %+v", hooks["Stop"])
	}
}

func TestRemoveHooksDeletesAFileThatWasOurs(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	if _, err := EnsureHooks(relay); err != nil {
		t.Fatal(err)
	}
	path, _ := HooksPath()
	if _, err := RemoveHooks(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("empty hooks file left behind: %v", err)
	}
}

func TestHooksPathUsesCodexHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	got, err := HooksPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(home, "hooks.json") {
		t.Fatalf("HooksPath = %q", got)
	}
}
