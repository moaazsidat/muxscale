package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/muxscale/muxscale/go/internal/adapter"
)

// Codex has no HTTP hook transport, so muxscale ships a command hook that
// relays the payload to the daemon. The command is repeated once per event;
// identifying it by this marker is what lets install and uninstall touch only
// muxscale's handlers in a file the user also owns.

// HooksPath is <CodexHome>/hooks.json.
func HooksPath() (string, error) {
	home, err := CodexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "hooks.json"), nil
}

// RelayCommand is what goes into every handler. The token file is a path, not
// a secret; the relay reads the token itself so it never appears in argv.
func RelayCommand(exe, addr, tokenFile string) string {
	cmd := fmt.Sprintf("%s hook-relay --agent codex --addr %s",
		shellQuote(exe), shellQuote(addr))
	if tokenFile != "" {
		cmd += " --token-file " + shellQuote(tokenFile)
	}
	return cmd
}

// hookSpec is one event we want wired, with the budget Codex allows.
type hookSpec struct {
	event   string
	timeout int
	status  string
}

var hookSpecs = []hookSpec{
	{"SessionStart", 10, ""},
	{"UserPromptSubmit", 10, ""},
	{"PreToolUse", 10, ""},
	{"PermissionRequest", 600, "muxscale · waiting for your phone…"},
	{"PostToolUse", 10, ""},
	{"Stop", 30, ""},
	{"PreCompact", 10, ""},
	{"PostCompact", 10, ""},
	{"SubagentStart", 10, ""},
	{"SubagentStop", 10, ""},
	{"Interrupt", 2, ""},
	{"SessionEnd", 2, ""},
}

// EnsureHooks adds muxscale's handlers to ~/.codex/hooks.json, preserving
// every hook already there. It returns true when the file changed.
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

// buildInstall computes the hooks file change without writing it.
func buildInstall(relay string) (path string, before, after []byte, err error) {
	path, err = HooksPath()
	if err != nil {
		return "", nil, nil, err
	}
	doc, raw, err := readHooksDoc(path)
	if err != nil {
		return "", nil, nil, err
	}
	hooks := hooksObject(doc)
	removeOurs(hooks, "")
	for _, spec := range hookSpecs {
		group := map[string]any{
			"hooks": []any{handlerFor(relay, spec)},
		}
		hooks[spec.event] = append(hookList(hooks[spec.event]), group)
	}
	want, err := marshalHooks(doc)
	if err != nil {
		return "", nil, nil, err
	}
	return path, raw, want, nil
}

// Plan is a pending hooks-file change, so a command can show it before
// mutating the user's Codex config.
type Plan struct {
	Path   string
	Before []byte
	After  []byte
}

func (p *Plan) Empty() bool { return string(p.Before) == string(p.After) }

func (p *Plan) Describe() string {
	if p.Empty() {
		return "Codex hooks are already up to date."
	}
	return fmt.Sprintf("write muxscale's command hooks to %s (backup: %s.muxscale.bak)",
		p.Path, p.Path)
}

func (p *Plan) Apply() error {
	if p.Empty() {
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

// planRelay is the command used by the Installer interface. The daemon passes
// its real address and token path at startup; this fallback is for a CLI
// caller and for conformance.
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
	hooks := hooksObject(doc)
	removeOurs(hooks, "")
	after, err := marshalHooks(doc)
	if err != nil {
		return nil, err
	}
	return &Plan{Path: path, Before: raw, After: after}, nil
}

func (*Adapter) Installed(context.Context) (bool, error) {
	return HooksInstalled(planRelay()), nil
}

func handlerFor(relay string, spec hookSpec) map[string]any {
	h := map[string]any{
		"type":    "command",
		"command": relay,
		"timeout": spec.timeout,
	}
	if spec.status != "" {
		h["statusMessage"] = spec.status
	}
	return h
}

// RemoveHooks removes only handlers whose command is muxscale's relay.
func RemoveHooks(relay string) (bool, error) {
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
	if len(raw) == 0 {
		return false, nil
	}
	hooks := hooksObject(doc)
	if !removeOurs(hooks, relay) {
		return false, nil
	}
	// The file was ours alone: remove it rather than leave an empty shell.
	if len(hooks) == 0 && len(doc) == 1 {
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

// HooksInstalled reports whether every event muxscale wires is present.
func HooksInstalled(relay string) bool {
	path, err := HooksPath()
	if err != nil {
		return false
	}
	doc, _, err := readHooksDoc(path)
	if err != nil {
		return false
	}
	hooks := hooksObject(doc)
	for _, spec := range hookSpecs {
		if !hasOurs(hookList(hooks[spec.event]), relay) {
			return false
		}
	}
	return true
}

// HooksPresent reports whether any muxscale handler is in the file, for
// uninstall lists that must not claim work they will not do.
func HooksPresent() bool {
	path, err := HooksPath()
	if err != nil {
		return false
	}
	doc, _, err := readHooksDoc(path)
	if err != nil {
		return false
	}
	hooks := hooksObject(doc)
	for _, list := range hooks {
		if hasOurs(hookList(list), "") {
			return true
		}
	}
	return false
}

// ---- JSON plumbing ----

func readHooksDoc(path string) (map[string]any, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{"hooks": map[string]any{}}, nil, nil
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

func hooksObject(doc map[string]any) map[string]any {
	if h, ok := doc["hooks"].(map[string]any); ok {
		return h
	}
	h := map[string]any{}
	doc["hooks"] = h
	return h
}

func hookList(v any) []any {
	if list, ok := v.([]any); ok {
		return list
	}
	return nil
}

func isOurs(handler any, relay string) bool {
	h, ok := handler.(map[string]any)
	if !ok {
		return false
	}
	cmd, _ := h["command"].(string)
	if !strings.Contains(cmd, "hook-relay --agent codex") {
		return false
	}
	return relay == "" || cmd == relay
}

func hasOurs(groups []any, relay string) bool {
	for _, g := range groups {
		group, ok := g.(map[string]any)
		if !ok {
			continue
		}
		for _, h := range hookList(group["hooks"]) {
			if isOurs(h, relay) {
				return true
			}
		}
	}
	return false
}

func removeOurs(hooks map[string]any, relay string) bool {
	changed := false
	for event, v := range hooks {
		groups := hookList(v)
		var keptGroups []any
		for _, g := range groups {
			group, ok := g.(map[string]any)
			if !ok {
				keptGroups = append(keptGroups, g)
				continue
			}
			var keptHandlers []any
			for _, h := range hookList(group["hooks"]) {
				if isOurs(h, relay) {
					changed = true
					continue
				}
				keptHandlers = append(keptHandlers, h)
			}
			if len(keptHandlers) == 0 {
				continue
			}
			group["hooks"] = keptHandlers
			keptGroups = append(keptGroups, group)
		}
		if len(keptGroups) == 0 {
			delete(hooks, event)
			continue
		}
		hooks[event] = keptGroups
	}
	return changed
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

// shellQuote makes one argument safe in the shell command Codex runs.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
