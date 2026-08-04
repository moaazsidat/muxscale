package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// PATH shims.
//
// This is what makes "just works" true rather than aspirational. The user keeps
// typing `claude`; the shim re-execs it under `muxscale run`, so the session is
// wrapped and can accept remote input. Without this, replying to a session
// requires the user to change a habit they have no reason to change.
//
// Two guards keep shims from being a nuisance:
//   - MUXSCALE_WRAPPED is set by the wrapper, so a shim never re-wraps itself
//     into infinite recursion.
//   - A shim only wraps when stdout is a TTY, so scripted and CI invocations
//     run the real binary untouched.

const shimMarker = "# muxscale-shim"

// ShimDir is where shims live.
func ShimDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".muxscale", "bin"), nil
}

// ShimPlan is a pending shim installation.
type ShimPlan struct {
	Dir string
	// Agents maps an agent binary name to the real path it resolves to.
	Agents map[string]string
	// Skipped records agents that are not installed on this machine.
	Skipped []string
	// PathHint is the line the user needs in their shell profile.
	PathHint string
	// ProfilePath is the rc file muxscale would edit, if the user consents.
	ProfilePath   string
	AlreadyOnPath bool
}

// shimTemplate re-execs the real binary under muxscale.
//
// `exec` matters: without it, the shim's shell would sit between the terminal
// and the agent, breaking job control and signal delivery.
const shimTemplate = `#!/bin/sh
%s
# Wraps %s so muxscale can monitor it and deliver remote replies.
# Remove this file, or unset the PATH entry, to disable.

REAL=%s

# Already inside a muxscale-wrapped session: run the real binary directly, or
# we would wrap ourselves forever.
if [ -n "$MUXSCALE_WRAPPED" ]; then
  exec "$REAL" "$@"
fi

# Not an interactive terminal (scripts, CI, pipes): stay out of the way.
if [ ! -t 1 ]; then
  exec "$REAL" "$@"
fi

# Explicit opt-out.
if [ "$MUXSCALE_SHIM" = "0" ]; then
  exec "$REAL" "$@"
fi

if command -v muxscale >/dev/null 2>&1; then
  exec muxscale run --attach -- "$REAL" "$@"
fi
exec "$REAL" "$@"
`

// BuildShimPlan works out which agents can be shimmed on this machine.
func BuildShimPlan(agents []string) (*ShimPlan, error) {
	dir, err := ShimDir()
	if err != nil {
		return nil, err
	}
	plan := &ShimPlan{Dir: dir, Agents: map[string]string{}}

	for _, a := range agents {
		real, err := resolveRealBinary(a, dir)
		if err != nil {
			plan.Skipped = append(plan.Skipped, a)
			continue
		}
		plan.Agents[a] = real
	}

	plan.PathHint = fmt.Sprintf(`export PATH="%s:$PATH"`, dir)
	plan.AlreadyOnPath = pathContains(dir)
	plan.ProfilePath = detectProfile()
	return plan, nil
}

// resolveRealBinary finds an agent's true path, skipping our own shim dir so
// re-running install cannot make a shim point at itself.
func resolveRealBinary(name, shimDir string) (string, error) {
	path := os.Getenv("PATH")
	var kept []string
	for _, p := range strings.Split(path, string(os.PathListSeparator)) {
		if p == "" || filepath.Clean(p) == filepath.Clean(shimDir) {
			continue
		}
		kept = append(kept, p)
	}

	old := os.Getenv("PATH")
	defer os.Setenv("PATH", old)
	_ = os.Setenv("PATH", strings.Join(kept, string(os.PathListSeparator)))

	real, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(real)
}

// Apply writes the shims.
func (p *ShimPlan) Apply() error {
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return err
	}
	for name, real := range p.Agents {
		content := fmt.Sprintf(shimTemplate, shimMarker, name, shellQuote(real))
		path := filepath.Join(p.Dir, name)
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			return fmt.Errorf("writing shim for %s: %w", name, err)
		}
	}
	return nil
}

// InstalledShims lists shims muxscale owns, identified by their marker so a
// user's own script of the same name is never removed.
func InstalledShims() ([]string, error) {
	dir, err := ShimDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || !strings.Contains(string(b), shimMarker) {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// UninstallShims removes only files carrying muxscale's marker.
func UninstallShims() ([]string, error) {
	dir, err := ShimDir()
	if err != nil {
		return nil, err
	}
	shims, err := InstalledShims()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, name := range shims {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return removed, err
		}
		removed = append(removed, name)
	}
	return removed, nil
}

func pathContains(dir string) bool {
	for _, p := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if p != "" && filepath.Clean(p) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

// detectProfile guesses the shell rc file to suggest. It is only ever a
// suggestion shown to the user -- muxscale does not edit it without consent.
func detectProfile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	shell := filepath.Base(os.Getenv("SHELL"))
	candidates := map[string][]string{
		"zsh":  {".zshrc"},
		"bash": {".bashrc", ".bash_profile"},
		"fish": {".config/fish/config.fish"},
	}
	for _, c := range candidates[shell] {
		p := filepath.Join(home, c)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if files, ok := candidates[shell]; ok && len(files) > 0 {
		return filepath.Join(home, files[0])
	}
	return filepath.Join(home, ".profile")
}

func shellQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
