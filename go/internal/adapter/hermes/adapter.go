package hermes

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Adapter is the Hermes Agent integration (Nous Research).
//
// Everything comes from hermes' SQLite state file. That makes the transcript
// exact and the state inference weak: rows land when messages are written, so
// silence is the only evidence that a turn ended, and silence is equally
// consistent with a model thinking for a long time.
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
		ID:          "hermes",
		DisplayName: "Hermes Agent",
		MinVersion:  MinVersion,
		// No ceiling: nothing above the floor has been tested against, and
		// inventing a NewestTested would badge every current install as
		// untested for no reason. Set this when a fixture is added.
		NewestTested: "",
		Signals: adapter.Signals{
			Discovery: adapter.SupportExact,
			// Silence is the only signal. Confidence stays low deliberately;
			// see InferState.
			State:      adapter.SupportDerived,
			Transcript: adapter.SupportExact,
			Approvals:  adapter.SupportNone,
		},
		Attachments: []protocol.Attachment{
			protocol.AttachWrap, protocol.AttachTmux, protocol.AttachTermctl,
		},
		ReadOnlyReason: "hermes records its conversation to a database but " +
			"exposes no channel to answer one; remote input needs the session " +
			"wrapped, in tmux, or in a terminal with remote control.",
	}
}

func (*Adapter) Detect(p adapter.Process) bool {
	return len(p.Argv) > 0 && filepath.Base(p.Argv[0]) == "hermes"
}

// Discover lists sessions from hermes' state database.
func (*Adapter) Discover(context.Context) ([]adapter.Discovered, error) {
	path, err := DBPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil // hermes isn't installed; not an error.
	}
	st, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer st.Close()

	sessions, err := st.Sessions(discoverLimit)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]adapter.Discovered, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, adapter.Discovered{
			NaturalKey: s.ID,
			// hermes does not record a working directory per session, so the
			// UI groups these by title rather than by project. Reported as
			// empty rather than guessed from the process, which would attach
			// the wrong project to a resumed session.
			Cwd:   "",
			Title: s.Title,
			Model: s.Model,
			Ref:   s.ID,
			Live:  s.IsActive(now),
		})
	}
	return out, nil
}

const discoverLimit = 200

// OpenTranscript streams a session's messages by rowid.
//
// The database connection is held open for the life of the stream rather than
// reopened per poll: SQLite in WAL mode handles a concurrent reader fine, and
// reopening would discard the rowid cursor's page cache on every tick.
func (*Adapter) OpenTranscript(_ context.Context, ref string) (adapter.TranscriptStream, error) {
	path, err := DBPath()
	if err != nil {
		return nil, err
	}
	st, err := Open(path)
	if err != nil {
		return nil, err
	}
	return &stream{store: st, sessionID: ref}, nil
}

type stream struct {
	store     *Store
	sessionID string
	cursor    int64
}

func (s *stream) Poll(context.Context) ([]protocol.TranscriptEvent, error) {
	events, next, err := s.store.Transcript(s.sessionID, s.cursor, pollLimit)
	if err != nil {
		return nil, err
	}
	s.cursor = next
	return events, nil
}

func (s *stream) Close() error { return s.store.Close() }

const pollLimit = 500

// InferState reconstructs just enough of the session row for the underlying
// heuristic.
//
// EndReason has to be carried through: it is hermes' only exact signal, and
// dropping it turns a finished session into "waiting for you" at 0.6
// confidence -- a notification for a session that will never do anything again.
func (a *Adapter) InferState(in adapter.StateInput) (protocol.StateGuess, bool) {
	s := Session{}
	if in.Ended {
		s.EndReason = in.EndReason
		if s.EndReason == "" {
			// IsLive keys off both fields; a session reported ended with no
			// stated reason must still read as ended.
			s.EndedAt = time.Now()
			s.EndReason = "ended"
		}
	}

	var last time.Time
	if n := len(in.Recent); n > 0 {
		last = in.Recent[n-1].At
	}
	if last.IsZero() && !in.Ended {
		return protocol.StateGuess{}, false
	}
	return InferState(s, last, in.Quiescence), true
}
