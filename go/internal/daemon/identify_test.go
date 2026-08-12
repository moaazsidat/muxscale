package daemon

import (
	"io"
	"log/slog"
	"testing"

	"github.com/muxscale/muxscale/go/internal/adapter/builtin"
	"github.com/muxscale/muxscale/go/internal/attach"
)

func identifier(t *testing.T) *Manager {
	t.Helper()
	return &Manager{
		adapters: builtin.Registry(builtin.Options{}),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestIdentifyUsesAdaptersThenTheBinaryTable(t *testing.T) {
	m := identifier(t)

	cases := []struct {
		name    string
		command string
		argv    []string
		want    string
	}{
		{"plain binary", "claude", []string{"claude", "--resume"}, "claude-code"},
		{"absolute path", "claude", []string{"/opt/homebrew/bin/claude"}, "claude-code"},
		{
			// The case the binary-name table structurally cannot see: argv[0]
			// is the runtime, so `comm` is "node" and nothing identifies it.
			"npm install runs under node",
			"node",
			[]string{"node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"},
			"claude-code",
		},
		{"codex", "codex", []string{"codex", "exec"}, "codex"},
		{"opencode", "opencode", []string{"opencode", "serve"}, "opencode"},
		{"pi", "pi", []string{"pi"}, "pi"},
		{"hermes", "hermes", []string{"hermes", "chat"}, "hermes"},

		// Agents with no adapter yet still resolve through the table, because
		// the process scan alone is worth something for them (spec §4.3.7).
		{"aider has no adapter", "aider", []string{"aider"}, "aider"},
		{"goose has no adapter", "goose", []string{"goose"}, "goose"},
		{"gemini maps to its id", "gemini", []string{"gemini"}, "gemini-cli"},

		// Must stay unclaimed.
		{"editing a file named after an agent", "vim", []string{"vim", "claude.go"}, ""},
		{"grep for an agent name", "grep", []string{"grep", "-r", "codex"}, ""},
		{"a wrapper is not the agent", "my-codex-tool", []string{"my-codex-tool"}, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := m.identify(c.command, c.argv); got != c.want {
				t.Errorf("identify(%q, %v) = %q, want %q",
					c.command, c.argv, got, c.want)
			}
		})
	}
}

// tmux reports pane_current_command and no argv at all.
func TestIdentifyWorksWithoutArgv(t *testing.T) {
	m := identifier(t)
	for cmd, want := range map[string]string{
		"claude":   "claude-code",
		"codex":    "codex",
		"opencode": "opencode",
		"vim":      "",
	} {
		if got := m.identify(cmd, nil); got != want {
			t.Errorf("identify(%q, nil) = %q, want %q", cmd, got, want)
		}
	}
}

// A Manager built without adapters must still identify what it can, so tests
// and any embedding that skips the built-in set keep working.
func TestIdentifyFallsBackWhenNoRegistry(t *testing.T) {
	m := &Manager{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if got := m.identify("claude", []string{"claude"}); got != "claude-code" {
		t.Errorf("got %q, want claude-code", got)
	}
	// ...but the argv-only case is exactly what it cannot see, which is the
	// reason the registry is wired in at all.
	if got := m.identify("node", []string{"node", "/x/@anthropic-ai/claude-code/cli.js"}); got != "" {
		t.Errorf("got %q; without adapters this case is not detectable", got)
	}
}

// The scan and the identifier must agree: a process the scan reports must be
// one the identifier can name, or it is collected and then silently dropped.
func TestScannedProcessesAreIdentifiable(t *testing.T) {
	m := identifier(t)
	procs, err := attach.Discover()
	if err != nil {
		t.Skipf("process scan unavailable: %v", err)
	}
	if len(procs) == 0 {
		t.Skip("no agents running on this machine")
	}
	for _, p := range procs {
		if got := m.identify(p.Command, p.Args); got == "" {
			t.Errorf("scan reported pid %d (%s, argv %v) but nothing identifies it",
				p.Pid, p.Command, p.Args)
		}
	}
	t.Logf("identified all %d scanned agent processes", len(procs))
}
