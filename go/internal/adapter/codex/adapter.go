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
)

func (*Adapter) Info() adapter.Info {
	return adapter.Info{
		ID:           "codex",
		DisplayName:  "Codex",
		MinVersion:   MinVersion,
		NewestTested: newestKnownProfile,
		Signals: adapter.Signals{
			Discovery: adapter.SupportExact,
			// Derived, not exact: the rollout is written as the turn proceeds,
			// so "waiting" is inferred from a trailing task_complete or from
			// silence, never from Codex telling us it is blocked.
			State:      adapter.SupportDerived,
			Transcript: adapter.SupportExact,
			Approvals:  adapter.SupportNone,
		},
		Attachments: []protocol.Attachment{
			protocol.AttachWrap, protocol.AttachTmux, protocol.AttachTermctl,
		},
		ReadOnlyReason: "Codex has no local API to answer an approval; " +
			"remote input needs the session to be wrapped, in tmux, or in a " +
			"terminal with remote control.",
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
	paths, err := FindRollouts(discoverScanLimit)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // Codex simply isn't installed.
		}
		return nil, err
	}
	out := make([]adapter.Discovered, 0, len(paths))
	for _, p := range paths {
		cwd, id, err := RolloutCwd(p)
		if err != nil {
			continue // A truncated or in-progress rollout is not an error.
		}
		fi, statErr := os.Stat(p)
		out = append(out, adapter.Discovered{
			NaturalKey: id,
			Cwd:        cwd,
			Ref:        p,
			Title:      strings.TrimSuffix(filepath.Base(p), ".jsonl"),
			Live:       statErr == nil && time.Since(fi.ModTime()) < liveWindow,
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

func (s *stream) Poll(context.Context) ([]protocol.TranscriptEvent, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	events, _, err := ParseRollout(f)
	if err != nil {
		return nil, err
	}
	if len(events) <= s.emitted {
		return nil, nil
	}
	fresh := events[s.emitted:]
	s.emitted = len(events)
	return fresh, nil
}

func (s *stream) Close() error { return nil }

// InferState uses the live event name when one is available and falls back to
// the rollout tail otherwise.
func (*Adapter) InferState(in adapter.StateInput) (protocol.StateGuess, bool) {
	if in.Event != "" {
		if g, ok := StateFromEvent(in.Event); ok {
			return g, true
		}
	}
	var last string
	if n := len(in.Recent); n > 0 {
		last = string(in.Recent[n-1].Kind)
	}
	return InferStateFromRollout(last, in.Idle, in.Quiescence), true
}
