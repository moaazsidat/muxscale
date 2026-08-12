// Package generic provides detection-only adapters.
//
// These are agents muxscale can find and, when they are in tmux or a
// controllable terminal, type into -- but whose conversation format has not
// been verified against a real installation. They occupy the bottom row of the
// spec §4.3.7 table: discovery yes, transcript and state no, remote input only
// via an attachment that does not depend on the agent cooperating.
//
// # Why these are not full adapters
//
// Writing a transcript parser from an agent's documentation is how the OpenCode
// adapter acquired a bug: the published OpenAPI advertised `directory`, `slug`
// and `version`, the running server returned null for all three and put the cwd
// in an undocumented `location` object. A struct written from the docs parsed
// cleanly and produced a blank working directory for every session. That was
// caught only because a real server was there to test against.
//
// None of the agents here are installed on the machine this was written on. A
// parser written from memory would be plausible, untestable, and wrong in ways
// that surface as an empty or subtly misordered transcript on a user's phone.
// Declaring no transcript is worse product and better engineering: the UI says
// "transcript not supported for this agent yet" instead of showing a confident
// blank.
//
// Promoting one of these is a contained change -- add the parser, flip Signals,
// implement Transcriber, and the conformance suite enforces that the
// declaration and the implementation agree.
package generic

import (
	"path/filepath"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Spec describes a detection-only agent.
type Spec struct {
	ID          string
	DisplayName string
	// Binaries are the argv[0] base names that identify this agent.
	Binaries []string
	// ArgvMarkers identify the agent when it runs under a runtime, so argv[0]
	// is `node` or `python`. Matched as substrings, so they must be specific
	// enough not to claim an unrelated process -- a package path, not a name.
	ArgvMarkers []string
	// MinVersion is the floor. Nominal for these: with no structured
	// integration there is nothing a version gates, but it keeps the field
	// meaningful when the adapter is promoted.
	MinVersion string
}

// Adapter is a detection-only integration.
type Adapter struct{ spec Spec }

// New returns an adapter for the given spec.
func New(s Spec) *Adapter { return &Adapter{spec: s} }

var _ adapter.Adapter = (*Adapter)(nil)

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{
		ID:          a.spec.ID,
		DisplayName: a.spec.DisplayName,
		MinVersion:  a.spec.MinVersion,
		Signals: adapter.Signals{
			// Derived, not exact: found by scanning the process table, so only
			// while running. A session that has exited leaves nothing behind
			// that muxscale knows how to read.
			Discovery:  adapter.SupportDerived,
			State:      adapter.SupportNone,
			Transcript: adapter.SupportNone,
			Approvals:  adapter.SupportNone,
		},
		// Input still works: wrap, tmux and termctl write to the terminal, and
		// need no cooperation from the agent. What is missing is knowing what
		// the agent said, or when it is waiting -- not the ability to type.
		Attachments: []protocol.Attachment{
			protocol.AttachWrap, protocol.AttachTmux, protocol.AttachTermctl,
		},
	}
}

func (a *Adapter) Detect(p adapter.Process) bool {
	if len(p.Argv) == 0 {
		return false
	}
	base := filepath.Base(p.Argv[0])
	for _, b := range a.spec.Binaries {
		if base == b {
			return true
		}
	}
	for _, arg := range p.Argv[1:] {
		for _, m := range a.spec.ArgvMarkers {
			if m != "" && contains(arg, m) {
				return true
			}
		}
	}
	return false
}

// contains avoids importing strings for one call in a hot path that runs once
// per process per scan.
func contains(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The detection-only set.
//
// Each of these has a real integration path documented in spec §4.5; they are
// here rather than there because none could be verified against a running
// installation. The comment on each says what it would take to promote it.
var (
	// Amp (Sourcegraph). Promotion needs its thread storage format.
	Amp = Spec{
		ID: "amp", DisplayName: "Amp",
		Binaries: []string{"amp"}, MinVersion: "0.1.0",
	}

	// Cursor CLI. Promotion needs the on-disk chat format.
	CursorAgent = Spec{
		ID: "cursor-agent", DisplayName: "Cursor CLI",
		Binaries: []string{"cursor-agent"}, MinVersion: "0.1.0",
	}

	// Aider. Writes .aider.chat.history.md in the project directory, which is
	// Markdown rather than structured -- promotion means deciding whether a
	// Markdown transcript is worth parsing or whether this stays here.
	Aider = Spec{
		ID: "aider", DisplayName: "Aider",
		Binaries:    []string{"aider"},
		ArgvMarkers: []string{"aider_chat", "aider/__main__"},
		MinVersion:  "0.1.0",
	}

	// Goose (Block). Promotion needs its session storage format.
	Goose = Spec{
		ID: "goose", DisplayName: "Goose",
		Binaries: []string{"goose"}, MinVersion: "0.1.0",
	}

	// Gemini CLI. Promotion needs the logs format under ~/.gemini.
	GeminiCLI = Spec{
		ID: "gemini-cli", DisplayName: "Gemini CLI",
		Binaries:    []string{"gemini"},
		ArgvMarkers: []string{"@google/gemini-cli"},
		MinVersion:  "0.1.0",
	}
)

// All returns every detection-only spec.
func All() []Spec { return []Spec{Amp, CursorAgent, Aider, Goose, GeminiCLI} }
