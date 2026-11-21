package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/muxscale/muxscale/go/internal/adapter"
)

// Antigravity loads global lifecycle hooks from ~/.gemini/config/hooks.json,
// keyed by a top-level hook name. Owning the "muxscale" top-level key lets
// install and uninstall merge cleanly alongside any user-authored hooks.
const (
	hookKey = "muxscale"
	marker  = "hook-relay --agent antigravity"
)

// HooksPath is ~/.gemini/config/hooks.json (or $GEMINI_CONFIG_DIR/hooks.json).
func HooksPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hooks.json"), nil
}

// RelayCommand is the shell command written into each Antigravity hook handler.
//
// `exec` matters: Antigravity runs hook commands via `sh -c`, and `exec`
// replaces the shell with `muxscale` so `os.Getppid()` inside `hook-relay` is
// the `agy` process itself (which links the hook session to its terminal).
func RelayCommand(exe, addr, tokenFile string) string {
	cmd := fmt.Sprintf("exec %s hook-relay --agent antigravity --addr %s",
		shellQuote(exe), shellQuote(addr))
	if tokenFile != "" {
		cmd += " --token-file " + shellQuote(tokenFile)
	}
	return cmd
}

// hookEvents are the observational flat-list events muxscale wires.
//
// PreToolUse is deliberately omitted: Antigravity treats an empty stdout `{}`
// from PreToolUse as a tool denial rather than fail-open, so wiring it would
// risk blocking tools whenever the daemon is down or has no opinion.
var hookEvents = []string{"SessionStart", "PreInvocation", "Stop"}

// EnsureHooks merges muxscale's hooks into ~/.gemini/config/hooks.json,
// preserving all other named hooks and taking a one-time backup before the
// first edit. Returns true when the file changed.
func EnsureHooks(relay string) (bool, error) {
	path, before, after, err := buildInstall(relay)
	if err != nil {
		return false, err
	}
	if string(before) == string(after) {
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
	if err := writeHooks(path, after); err != nil {
		return false, err
	}
	return true, nil
}

func buildInstall(relay string) (path string, before, after []byte, err error) {
	path, err = HooksPath()
	if err != nil {
		return "", nil, nil, err
	}
	doc, raw, err := readHooksDoc(path)
	if err != nil {
		return "", nil, nil, err
	}
	stanza := map[string]any{}
	for _, ev := range hookEvents {
		stanza[ev] = []any{
			map[string]any{
				"type":    "command",
				"command": relay,
				"timeout": 10,
			},
		}
	}
	doc[hookKey] = stanza
	want, err := marshalHooks(doc)
	if err != nil {
		return "", nil, nil, err
	}
	return path, raw, want, nil
}

// Plan is a pending change to ~/.gemini/config/hooks.json.
type Plan struct {
	Path   string
	Before []byte
	After  []byte
	Remove bool
}

func (p *Plan) Empty() bool { return !p.Remove && string(p.Before) == string(p.After) }

func (p *Plan) Describe() string {
	if p.Empty() {
		return "Antigravity hooks are already up to date."
	}
	if p.Remove {
		return fmt.Sprintf("remove %s (contained only muxscale's hooks)", p.Path)
	}
	return fmt.Sprintf("write muxscale's command hooks to %s (backup: %s.muxscale.bak)",
		p.Path, p.Path)
}

func (p *Plan) Apply() error {
	if p.Empty() {
		return nil
	}
	if p.Remove {
		if err := os.Remove(p.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if len(p.Before) > 0 {
		backup := p.Path + ".muxscale.bak"
		if _, err := os.Stat(backup); os.IsNotExist(err) {
			if err := os.WriteFile(backup, p.Before, 0o600); err != nil {
				return err
			}
		}
	}
	return writeHooks(p.Path, p.After)
}

func planRelay() string {
	exe, err := os.Executable()
	if err != nil {
		exe = "muxscale"
	}
	addr := os.Getenv("MUXSCALE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:4455"
	}
	return RelayCommand(exe, addr, os.Getenv("MUXSCALE_HOOK_TOKEN_FILE"))
}

func (*Adapter) PlanInstall(context.Context) (adapter.InstallPlan, error) {
	path, before, after, err := buildInstall(planRelay())
	if err != nil {
		return nil, err
	}
	return &Plan{Path: path, Before: before, After: after}, nil
}

func (*Adapter) PlanUninstall(context.Context) (adapter.InstallPlan, error) {
	path, err := HooksPath()
	if err != nil {
		return nil, err
	}
	doc, raw, err := readHooksDoc(path)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || !removeOurs(doc) {
		return &Plan{Path: path, Before: raw, After: raw}, nil
	}
	if len(doc) == 0 {
		return &Plan{Path: path, Before: raw, Remove: true}, nil
	}
	after, err := marshalHooks(doc)
	if err != nil {
		return nil, err
	}
	return &Plan{Path: path, Before: raw, After: after}, nil
}

func (*Adapter) Installed(context.Context) (bool, error) {
	return HooksInstalled(planRelay()), nil
}

// RemoveHooks removes muxscale's hook entry from ~/.gemini/config/hooks.json,
// deleting the file if it contained nothing else. Returns true when changed.
func RemoveHooks() (bool, error) {
	path, err := HooksPath()
	if err != nil {
		return false, err
	}
	doc, raw, err := readHooksDoc(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if len(raw) == 0 || !removeOurs(doc) {
		return false, nil
	}
	if len(doc) == 0 {
		if err := os.Remove(path); err != nil {
			return false, err
		}
		return true, nil
	}
	want, err := marshalHooks(doc)
	if err != nil {
		return false, err
	}
	return true, writeHooks(path, want)
}

// HooksInstalled reports whether every event muxscale wires is present with
// the expected relay command.
func HooksInstalled(relay string) bool {
	path, err := HooksPath()
	if err != nil {
		return false
	}
	doc, _, err := readHooksDoc(path)
	if err != nil {
		return false
	}
	stanza, ok := doc[hookKey].(map[string]any)
	if !ok {
		return false
	}
	for _, ev := range hookEvents {
		if !hasOurs(stanza[ev], relay) {
			return false
		}
	}
	return true
}

// HooksPresent reports whether muxscale has any entry in hooks.json.
func HooksPresent() bool {
	path, err := HooksPath()
	if err != nil {
		return false
	}
	doc, _, err := readHooksDoc(path)
	if err != nil {
		return false
	}
	stanza, ok := doc[hookKey].(map[string]any)
	if !ok {
		return false
	}
	for _, v := range stanza {
		if hasOurs(v, "") {
			return true
		}
	}
	return false
}

func readHooksDoc(path string) (map[string]any, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil, nil
		}
		return nil, nil, err
	}
	doc := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
		}
	}
	return doc, raw, nil
}

func hasOurs(v any, relay string) bool {
	list, ok := v.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		h, ok := item.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := h["command"].(string)
		if !strings.Contains(cmd, marker) {
			continue
		}
		if relay == "" || cmd == relay {
			return true
		}
	}
	return false
}

func removeOurs(doc map[string]any) bool {
	stanza, ok := doc[hookKey].(map[string]any)
	if !ok {
		return false
	}
	found := false
	for _, v := range stanza {
		if hasOurs(v, "") {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	delete(doc, hookKey)
	return true
}

func marshalHooks(doc map[string]any) ([]byte, error) {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func writeHooks(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hooks-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
