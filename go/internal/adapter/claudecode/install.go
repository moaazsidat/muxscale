package claudecode

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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

// TokenEnvVar is the variable the hooks send as their bearer token.
//
// Install puts it in settings.json's env block, which Claude Code copies into
// its own environment at startup, rather than leaving it to a line in a shell
// profile: that only reached sessions started from a shell that had sourced
// it, and a session started any other way -- or before the line was added --
// had every hook rejected.
const TokenEnvVar = "MUXSCALE_HOOK_TOKEN"

// InstallOptions configures hook installation.
type InstallOptions struct {
	SettingsPath string // default ~/.claude/settings.json
	Endpoint     string // e.g. http://127.0.0.1:4455/v1/hook/claude-code
	TokenEnvVar  string // default TokenEnvVar
	// Token, when set, is written into settings.json's env block under
	// TokenEnvVar, so the hooks can authenticate with no shell setup.
	Token        string
	HoldSeconds  int // decision-hook timeout; must exceed the daemon's hold
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
	if opt.TokenEnvVar == "" {
		opt.TokenEnvVar = TokenEnvVar
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
			// Claude Code waits for an HTTP hook's answer: unlike command
			// hooks, they have no async mode, and an "async" key is dropped
			// the next time it rewrites the file. The daemon answers these at
			// once, so the wait is a round trip on loopback; the short ceiling
			// bounds how long a hung daemon could stall each tool call.
			h.Timeout = 5
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
	if opt.Token != "" {
		env, _ := root["env"].(map[string]any)
		if env == nil {
			env = map[string]any{}
		}
		env[opt.TokenEnvVar] = opt.Token
		root["env"] = env
	}
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
	if env, ok := root["env"].(map[string]any); ok {
		if _, ours := env[TokenEnvVar]; ours {
			delete(env, TokenEnvVar)
			plan.Removing = append(plan.Removing, "env "+TokenEnvVar)
		}
		if len(env) == 0 {
			delete(root, "env")
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
// Distinguished from a real change so a caller can say "already
// configured" rather than showing a diff with nothing in it.
func (p *Plan) Empty() bool { return len(p.Adding) == 0 && len(p.Removing) == 0 }

// Changes reports whether applying the plan would change what settings.json
// says. Compared as JSON values, not text, so a file someone formatted by hand
// is not rewritten -- and backed up -- on every daemon start for no reason.
func (p *Plan) Changes() bool {
	if p.Before == "" {
		return true
	}
	var before, after any
	if json.Unmarshal([]byte(p.Before), &before) != nil || json.Unmarshal([]byte(p.After), &after) != nil {
		return true
	}
	// The "//" tags are left out: Claude Code strips them whenever it rewrites
	// the file, and putting them back is not worth a rewrite and a backup on
	// every start.
	return !reflect.DeepEqual(untagged(before), untagged(after))
}

// untagged returns v without "//" keys, at any depth.
func untagged(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if k != "//" {
				out[k] = untagged(x)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = untagged(x)
		}
		return out
	}
	return v
}

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
	// Also match on the nested handler: by its tag, in case the outer comment
	// was edited, and by where it sends, because the tags do not survive.
	// Claude Code drops keys it does not know when it rewrites settings.json,
	// and an install that recognised its entries only by tag found none and
	// added a second set beside the first -- every hook then fired twice.
	hooks, _ := m["hooks"].([]any)
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if c, _ := hm["//"].(string); c == managedTag {
			return true
		}
		if u, _ := hm["url"].(string); isHookEndpoint(u) {
			return true
		}
	}
	return false
}

// hookPath is the daemon's hook endpoint path, on whatever address it serves.
const hookPath = "/v1/hook/claude-code"

// isHookEndpoint reports whether a hook URL points at a muxscale daemon.
func isHookEndpoint(u string) bool {
	parsed, err := url.Parse(u)
	return err == nil && parsed.Path == hookPath
}

func toMap(h hookHandler) map[string]any {
	b, _ := json.Marshal(h)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

// EnsureHooks makes settings.json say what a running daemon needs: muxscale's
// hooks pointed at endpoint, and the token they authenticate with. The daemon
// calls it on every start, which is what makes hooks part of running muxscale
// rather than a step someone has to know to take.
//
// It writes only when something differs -- first run, a new token, a moved
// endpoint, an entry someone deleted -- and then backs the file up first, as
// install always has. The applied plan is returned so the caller can say what
// changed; nil means settings.json was already right.
//
// Below the supported floor it changes nothing and says why: hooks on an
// older Claude Code are a weaker safety model, not a degraded one.
func EnsureHooks(endpoint, token string) (*Plan, error) {
	v, err := DetectVersion()
	if err != nil {
		return nil, fmt.Errorf("Claude Code not found, so no hooks: %w", err)
	}
	if Classify(v) == SupportNone {
		return nil, fmt.Errorf("%s", SupportMessage(v, SupportNone))
	}
	plan, err := BuildInstall(InstallOptions{
		Endpoint:     endpoint,
		Token:        token,
		Events:       ObservationalEvents,
		DecisionEvts: DecisionEvents,
	})
	if err != nil {
		return nil, err
	}
	if !plan.Changes() {
		return nil, nil
	}
	if err := plan.Apply(); err != nil {
		return nil, err
	}
	return plan, nil
}
