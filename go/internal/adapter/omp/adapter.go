package omp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Adapter is the omp integration.
//
// It declares exact transcript and discovery because both are read from files
// omp writes, not guessed from a screen. State is exact once the extension is
// installed and derived otherwise; the same shape Claude Code has had since
// its hooks landed. Approvals are declared none: no extension API can answer
// omp's own approval dialog, and promising a reply that silently does nothing
// is worse than saying so (see docs/20261018-omp-adapter.md §9).
type Adapter struct{}

// New returns the omp adapter.
func New() *Adapter { return &Adapter{} }

var (
	_ adapter.Adapter       = (*Adapter)(nil)
	_ adapter.Discoverer    = (*Adapter)(nil)
	_ adapter.Transcriber   = (*Adapter)(nil)
	_ adapter.StateInferrer = (*Adapter)(nil)
	_ adapter.Locator       = (*Adapter)(nil)
	_ adapter.Liveness      = (*Adapter)(nil)
)

func (*Adapter) Info() adapter.Info {
	return adapter.Info{
		ID:           "omp",
		DisplayName:  "omp",
		MinVersion:   MinVersion,
		NewestTested: NewestTested,
		Signals: adapter.Signals{
			// Session files exist after the process is gone, so discovery does
			// not depend on catching omp while it runs.
			Discovery: adapter.SupportExact,
			// The extension reports turn boundaries; without it the transcript
			// and the writer lease give a capped guess. Same honesty rule as
			// Claude Code before its hooks are installed.
			State:      adapter.SupportExact,
			Transcript: adapter.SupportExact,
			// No structured approval channel exists in the extension API.
			Approvals: adapter.SupportNone,
		},
		Attachments: []protocol.Attachment{
			protocol.AttachWrap, protocol.AttachTmux, protocol.AttachTermctl,
		},
	}
}

func (*Adapter) Detect(p adapter.Process) bool {
	return len(p.Argv) > 0 && base(p.Argv[0]) == "omp"
}

func base(s string) string {
	return strings.TrimSuffix(filepath.Base(s), ".exe")
}

// Discover walks ~/.omp/agent/sessions; see Layout.Discover.
func (*Adapter) Discover(context.Context) ([]adapter.Discovered, error) {
	return OmpLayout.Discover()
}

// OpenTranscript follows a session file. ref is the transcript path.
func (*Adapter) OpenTranscript(_ context.Context, ref string) (adapter.TranscriptStream, error) {
	return newStream(ref), nil
}

// Live is adapter.Liveness: whether omp's writer lease is still held.
func (*Adapter) Live(ref string) (bool, bool) { return Live(ref) }

// TranscriptForProcess reads the terminal breadcrumb, which is a fact omp's
// own runtime wrote: the session file this tty is showing.
//
// The first line is the cwd; a breadcrumb whose cwd disagrees with the process
// belongs to a previous occupant of the tty and is ignored.
func (*Adapter) TranscriptForProcess(p adapter.Process) string {
	if p.TTY == "" {
		return ""
	}
	b, err := breadcrumbPath(p.TTY)
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(b)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	if len(lines) < 2 {
		return ""
	}
	cwd := strings.TrimSpace(lines[0])
	path := strings.TrimSpace(lines[1])
	if !filepath.IsAbs(path) || !strings.HasSuffix(path, ".jsonl") {
		return ""
	}
	if p.Cwd != "" && cwd != "" && canonical(p.Cwd) != canonical(cwd) {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// MatchTranscript joins a process to a project's session files when no
// breadcrumb exists; see Layout.MatchTranscript.
func (*Adapter) MatchTranscript(cwd string, started time.Time, claimed map[string]bool) string {
	return OmpLayout.MatchTranscript(cwd, started, claimed)
}

// canonical resolves symlinks when it can, falling back to a clean path. A
// directory that no longer exists is still comparable by its spelling.
func canonical(path string) string {
	if r, err := filepath.EvalSymlinks(filepath.Clean(path)); err == nil {
		return r
	}
	return filepath.Clean(path)
}

// DetectVersion runs `omp --version`, which prints "omp v18.12.2".
func DetectVersion() (adapter.Version, error) {
	out, err := exec.Command("omp", "--version").Output()
	if err != nil {
		return adapter.Version{}, err
	}
	return adapter.ParseVersion(string(out))
}

// InferState derives state from the transcript, the writer lease, and silence.
//
// Confidence is deliberately below the extension's: this is inference, and
// the product's credibility depends on not dressing a guess up as a fact.
func (*Adapter) InferState(in adapter.StateInput) (protocol.StateGuess, bool) {
	return InferTreeState(in, "omp")
}

// InferTreeState is InferState for any agent writing this session format;
// agent names it in the evidence.
func InferTreeState(in adapter.StateInput, agent string) (protocol.StateGuess, bool) {
	if len(in.Recent) == 0 {
		return protocol.StateGuess{}, false
	}
	now := time.Now()

	// omp writes a session_exit record whenever it disposes a session. It is
	// the only exact statement the file itself makes about the end.
	for i := len(in.Recent) - 1; i >= 0 && i >= len(in.Recent)-4; i-- {
		e := in.Recent[i]
		if e.Kind != protocol.EventStateChange || !strings.HasPrefix(e.Text, "session_exit ") {
			continue
		}
		state, reason := protocol.StateExited, "omp recorded the session exiting"
		if e.IsError || strings.Contains(e.Text, "fatal") || strings.Contains(e.Text, "signal") {
			state, reason = protocol.StateCrashed, "omp recorded the session failing"
		}
		return protocol.StateGuess{
			State: state, Confidence: 0.9, Reason: reason,
			Evidence: e.Text, At: now,
		}, true
	}

	if in.Live != nil && !*in.Live {
		return protocol.StateGuess{
			State: protocol.StateExited, Confidence: 0.85,
			Reason: "omp released the session's writer lease", Evidence: "lease:free", At: now,
		}, true
	}

	last, ok := lastTurnEvent(in.Recent)
	if !ok {
		return protocol.StateGuess{}, false
	}
	switch last.Kind {
	case protocol.EventToolCall:
		return guess(agent, protocol.StateRunning, 0.6, "a tool call is in flight", last, now), true
	case protocol.EventToolResult:
		return guess(agent, protocol.StateRunning, 0.55, "a tool result just landed; the model speaks next", last, now), true
	case protocol.EventUserMessage, protocol.EventCommand:
		return guess(agent, protocol.StateRunning, 0.5, "the last entry is input to the agent", last, now), true
	case protocol.EventCompaction:
		return guess(agent, protocol.StateRunning, 0.5, "omp is compacting the conversation", last, now), true
	case protocol.EventAssistantMessage, protocol.EventThinking, protocol.EventError:
		if in.Idle >= in.Quiescence {
			return guess(agent, protocol.StateWaitingInput, 0.7,
				"the assistant finished speaking and the transcript has been quiet", last, now), true
		}
		return guess(agent, protocol.StateRunning, 0.65, "the assistant is mid-turn", last, now), true
	}
	return protocol.StateGuess{}, false
}

// lastTurnEvent is the newest event that says whose turn it is.
//
// omp follows the end of a turn with bookkeeping -- a mode change after the
// final reply, a goal or todo record, a usage tally -- and judged on that
// alone, a session that had finished answering kept the state of its last
// tool result: Working, for as long as it waited for its person.
func lastTurnEvent(recent []protocol.TranscriptEvent) (protocol.TranscriptEvent, bool) {
	for i := len(recent) - 1; i >= 0; i-- {
		switch recent[i].Kind {
		case protocol.EventNotice, protocol.EventTokenUsage, protocol.EventStateChange:
			continue
		}
		return recent[i], true
	}
	return protocol.TranscriptEvent{}, false
}

func guess(agent string, s protocol.SessionState, c float64, reason string, last protocol.TranscriptEvent, at time.Time) protocol.StateGuess {
	return protocol.StateGuess{
		State: s, Confidence: c, Reason: reason,
		Evidence: agent + " transcript: last event " + string(last.Kind), At: at,
	}
}
