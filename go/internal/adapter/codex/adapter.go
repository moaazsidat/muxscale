package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Adapter is the Codex integration.
//
// Read-only by design, not by omission: Codex persists rollout files and offers
// no local channel to answer a question. The approval events in a rollout are
// visible *after* the fact, so muxscale can show that Codex asked something --
// it cannot carry the answer back. Declaring that honestly in Signals is what
// stops the UI offering a reply button that would silently do nothing.
type Adapter struct{}

func New() *Adapter { return &Adapter{} }

var (
	_ adapter.Adapter       = (*Adapter)(nil)
	_ adapter.Discoverer    = (*Adapter)(nil)
	_ adapter.Transcriber   = (*Adapter)(nil)
	_ adapter.StateInferrer = (*Adapter)(nil)
	_ adapter.EventReporter = (*stream)(nil)
	_ adapter.Installer     = (*Adapter)(nil)
)

func (*Adapter) Info() adapter.Info {
	return adapter.Info{
		ID:           "codex",
		DisplayName:  "Codex",
		MinVersion:   MinVersion,
		NewestTested: newestKnownProfile,
		Signals: adapter.Signals{
			Discovery: adapter.SupportExact,
			// Hooks report the turn boundary and approval requests; without
			// them the rollout's task_complete is a 0.9 inference. Same shape
			// as Claude Code before its hooks are installed.
			State:      adapter.SupportExact,
			Transcript: adapter.SupportExact,
			// PermissionRequest is held open by the relay until a human
			// answers. The hooks must be trusted once in /hooks; until then
			// the read path still works and state is inferred.
			Approvals: adapter.SupportExact,
		},
		Attachments: []protocol.Attachment{
			protocol.AttachWrap, protocol.AttachTmux, protocol.AttachTermctl,
		},
		ReadOnlyReason: "Codex approvals and state need its lifecycle hooks " +
			"installed and trusted; remote input needs the session to be " +
			"wrapped, in tmux, or in a terminal with remote control.",
	}
}

func (*Adapter) Detect(p adapter.Process) bool {
	return len(p.Argv) > 0 && filepath.Base(p.Argv[0]) == "codex"
}

// Discover enumerates recent rollout files.
//
// Codex nests sessions under YYYY/MM/DD rather than by project, so there is no
// path-derived way to find a directory's session; the cwd comes from reading
// each file's header. The scan is capped for that reason -- it is proportional
// to files opened, not to sessions that matter.
func (*Adapter) Discover(context.Context) ([]adapter.Discovered, error) {
	// The thread index knows titles, renames and projects without opening a
	// rollout. Fall back to the files when it is absent or from a schema this
	// build does not know.
	if out, ok := discoveredFromThreads(discoverScanLimit); ok {
		return out, nil
	}
	paths, err := FindRollouts(discoverScanLimit)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // Codex simply isn't installed.
		}
		return nil, err
	}
	out := make([]adapter.Discovered, 0, len(paths))
	for _, p := range paths {
		sm, err := readSessionMeta(p)
		if err != nil || sm.ID == "" {
			continue // A truncated or in-progress rollout is not an error.
		}
		kind, _ := decodeSource(sm.Source)
		fi, statErr := os.Stat(p)
		out = append(out, adapter.Discovered{
			NaturalKey: sm.ID,
			Cwd:        sm.Cwd,
			Ref:        p,
			Title:      strings.TrimSuffix(filepath.Base(p), ".jsonl"),
			Live:       statErr == nil && time.Since(fi.ModTime()) < liveWindow && kind != sourceExec,
		})
	}
	return out, nil
}

const (
	discoverScanLimit = 200
	// liveWindow is how recently a rollout must have changed to be called live.
	// Generous, because a session waiting on a human is idle by definition and
	// calling it dead would remove exactly the sessions worth notifying about.
	liveWindow = 12 * time.Hour
)

func (*Adapter) OpenTranscript(_ context.Context, ref string) (adapter.TranscriptStream, error) {
	return &stream{path: ref}, nil
}

// stream re-reads the rollout and returns the events it has not yet emitted.
//
// Re-parsing the whole file each poll rather than seeking is deliberate:
// rollout records are not fixed-width, a partially-flushed final line is
// normal, and a byte offset into a file being appended by another process is
// the kind of optimisation that yields a corrupt event once a week. Rollouts
// are bounded by a single session, so the cost is small and predictable.
type stream struct {
	path    string
	emitted int
	lastEvt string
}

// LastEvent is the Codex lifecycle record that state inference keys on. It is
// not a rendered event: task_complete produces no transcript event.
func (s *stream) LastEvent() string { return s.lastEvt }

func (s *stream) Poll(context.Context) ([]protocol.TranscriptEvent, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	events, meta, err := ParseRollout(f)
	if err != nil {
		return nil, err
	}
	if meta.LastEventType != "" {
		s.lastEvt = meta.LastEventType
	}
	if len(events) <= s.emitted {
		return nil, nil
	}
	fresh := events[s.emitted:]
	s.emitted = len(events)
	return fresh, nil
}

func (s *stream) Close() error { return nil }

// InferState uses the lifecycle record when the stream can name one, and falls
// back to the rendered tail for an older stream or a hand-built input.
func (*Adapter) InferState(in adapter.StateInput) (protocol.StateGuess, bool) {
	if in.Event != "" {
		return InferStateFromRollout(in.Event, in.Idle, in.Quiescence), true
	}
	if n := len(in.Recent); n > 0 {
		switch in.Recent[n-1].Kind {
		case protocol.EventToolCall, protocol.EventToolResult, protocol.EventUserMessage:
			return protocol.StateGuess{
				State: protocol.StateRunning, Confidence: 0.6,
				Reason: "the rollout ends with activity", Evidence: "codex:rollout tail", At: time.Now(),
			}, true
		case protocol.EventAssistantMessage, protocol.EventThinking:
			state := protocol.StateRunning
			confidence := 0.6
			if in.Idle >= in.Quiescence {
				state, confidence = protocol.StateWaitingInput, 0.6
			}
			return protocol.StateGuess{
				State: state, Confidence: confidence,
				Reason: "the rollout ends with an assistant turn", Evidence: "codex:rollout tail", At: time.Now(),
			}, true
		}
	}
	return InferStateFromRollout("", in.Idle, in.Quiescence), true
}
