package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Hook installation into ~/.claude/settings.json.
//
// Two rules govern this file, both because it is the user's, not ours:
//  1. Never clobber. muxscale's entries are tagged and appended; existing hooks
//     are preserved byte-for-byte via a generic map round-trip.
//  2. Always reversible. Uninstall removes exactly our tagged entries, and a
//     timestamped backup is written before any change.

// managedTag marks entries muxscale owns. Uninstall matches on it.
const managedTag = "muxscale-managed"

// InstallOptions configures hook installation.
type InstallOptions struct {
	SettingsPath string // default ~/.claude/settings.json
	Endpoint     string // e.g. http://127.0.0.1:4455/v1/hook/claude-code
	TokenEnvVar  string // e.g. MUXSCALE_HOOK_TOKEN
	HoldSeconds  int    // decision-hook timeout; must exceed the daemon's hold
	Events       []string
	DecisionEvts []string
}

// DefaultSettingsPath is ~/.claude/settings.json.
func DefaultSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// hookEntry is one matcher group in settings.json.
type hookEntry struct {
	Matcher string         `json:"matcher,omitempty"`
	Hooks   []hookHandler  `json:"hooks"`
	Comment string         `json:"//,omitempty"`
	Extra   map[string]any `json:"-"`
}

type hookHandler struct {
	Type           string            `json:"type"`
	URL            string            `json:"url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	AllowedEnvVars []string          `json:"allowedEnvVars,omitempty"`
	Timeout        int               `json:"timeout,omitempty"`
	StatusMessage  string            `json:"statusMessage,omitempty"`
	Async          bool              `json:"async,omitempty"`
	Comment        string            `json:"//,omitempty"`
}

// Plan describes a pending change, so it can be shown before it is applied.
// Every command that mutates the user's machine prints the exact change first.
type Plan struct {
	SettingsPath string
	BackupPath   string
	Adding       []string
	Removing     []string
	Before       string
	After        string
}

// BuildInstall computes the settings.json change without writing it.
func BuildInstall(opt InstallOptions) (*Plan, error) {
	if opt.SettingsPath == "" {
		p, err := DefaultSettingsPath()
		if err != nil {
			return nil, err
		}
		opt.SettingsPath = p
	}
	if opt.HoldSeconds == 0 {
		opt.HoldSeconds = 600
	}

	root, before, err := loadSettings(opt.SettingsPath)
	if err != nil {
		return nil, err
	}

	hooks := map[string]any{}
	if h, ok := root["hooks"].(map[string]any); ok {
		hooks = h
	}

	plan := &Plan{SettingsPath: opt.SettingsPath, Before: before}

	add := func(event string, isDecision bool) {
		existing, _ := hooks[event].([]any)
		// Drop any prior muxscale entry so install is idempotent.
		kept := make([]any, 0, len(existing))
		for _, e := range existing {
			if !isManaged(e) {
				kept = append(kept, e)
			}
		}

		h := hookHandler{
			Type:           "http",
			URL:            opt.Endpoint,
			Headers:        map[string]string{"Authorization": "Bearer $" + opt.TokenEnvVar},
			AllowedEnvVars: []string{opt.TokenEnvVar},
			Comment:        managedTag,
		}
		if isDecision {
			// Held open while a human decides. The daemon answers well before
			// this; the generous ceiling only prevents Claude Code from timing
			// out first and producing a confusing double-prompt.
			h.Timeout = opt.HoldSeconds
			h.StatusMessage = "muxscale · waiting for your phone…"
		} else {
			h.Async = true // never sit in the agent's critical path
			h.Timeout = 10
		}

		entry := map[string]any{
			"matcher": ".*",
			"//":      managedTag,
			"hooks":   []any{toMap(h)},
		}
		hooks[event] = append(kept, entry)
		plan.Adding = append(plan.Adding, event)
	}

	for _, e := range opt.Events {
		add(e, false)
	}
	for _, e := range opt.DecisionEvts {
		add(e, true)
	}

	root["hooks"] = hooks
	after, err := marshalSettings(root)
	if err != nil {
		return nil, err
	}
	plan.After = after
	return plan, nil
}

// BuildUninstall computes removal of exactly muxscale's entries.
func BuildUninstall(settingsPath string) (*Plan, error) {
	if settingsPath == "" {
		p, err := DefaultSettingsPath()
		if err != nil {
			return nil, err
		}
		settingsPath = p
	}
	root, before, err := loadSettings(settingsPath)
	if err != nil {
		return nil, err
	}
	plan := &Plan{SettingsPath: settingsPath, Before: before}

	if hooks, ok := root["hooks"].(map[string]any); ok {
		for event, v := range hooks {
			list, ok := v.([]any)
			if !ok {
				continue
			}
			kept := make([]any, 0, len(list))
			for _, e := range list {
				if isManaged(e) {
					plan.Removing = append(plan.Removing, event)
					continue
				}
				kept = append(kept, e)
			}
			if len(kept) == 0 {
				delete(hooks, event)
			} else {
				hooks[event] = kept
			}
		}
		if len(hooks) == 0 {
			delete(root, "hooks")
		}
	}

	after, err := marshalSettings(root)
	if err != nil {
		return nil, err
	}
	plan.After = after
	return plan, nil
}

// Empty reports that settings.json already says what we would write.
// Distinguished from a real change so `muxscale up` can say "already
// configured" rather than showing a diff with nothing in it.
func (p *Plan) Empty() bool { return len(p.Adding) == 0 && len(p.Removing) == 0 }

// Describe renders the pending change for a human. Callers show this and get
// confirmation before Apply; nothing here touches the disk.
func (p *Plan) Describe() string {
	if p.Empty() {
		return fmt.Sprintf("%s is already configured; nothing to change.", p.SettingsPath)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", p.SettingsPath)
	for _, h := range p.Adding {
		fmt.Fprintf(&b, "  + %s\n", h)
	}
	for _, h := range p.Removing {
		fmt.Fprintf(&b, "  - %s\n", h)
	}
	if p.Before != "" {
		fmt.Fprintf(&b, "  the current file is backed up alongside it first\n")
	}
	return b.String()
}

// Apply writes the plan, backing up the original first.
func (p *Plan) Apply() error {
	if p.Before != "" {
		p.BackupPath = fmt.Sprintf("%s.muxscale-backup-%s",
			p.SettingsPath, time.Now().UTC().Format("20060102T150405Z"))
		if err := os.WriteFile(p.BackupPath, []byte(p.Before), 0o600); err != nil {
			return fmt.Errorf("writing backup: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(p.SettingsPath), 0o755); err != nil {
		return err
	}
	// Write-then-rename so a crash mid-write cannot leave the user with a
	// truncated settings.json and a broken Claude Code.
	tmp := p.SettingsPath + ".muxscale-tmp"
	if err := os.WriteFile(tmp, []byte(p.After), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.SettingsPath)
}

func loadSettings(path string) (map[string]any, string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	root := map[string]any{}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &root); err != nil {
			return nil, "", fmt.Errorf("%s is not valid JSON (refusing to touch it): %w", path, err)
		}
	}
	return root, string(b), nil
}

func marshalSettings(root map[string]any) (string, error) {
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

func isManaged(e any) bool {
	m, ok := e.(map[string]any)
	if !ok {
		return false
	}
	if c, _ := m["//"].(string); c == managedTag {
		return true
	}
	// Also match on the nested handler, in case the outer comment was edited.
	hooks, _ := m["hooks"].([]any)
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if c, _ := hm["//"].(string); c == managedTag {
			return true
		}
	}
	return false
}

func toMap(h hookHandler) map[string]any {
	b, _ := json.Marshal(h)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}
