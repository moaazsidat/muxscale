package opencode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Adapter is the OpenCode integration.
//
// Two channels. The database (db.go) is the one every session has: discovery,
// the whole transcript, and state inferred from how the last turn ended,
// including for sessions started before muxscale. The HTTP server is the
// other: an SSE event stream and REST endpoints for prompting, answering
// permissions and interrupting -- but the TUI's server listens on a port it
// tells nobody, so that channel is only reachable for `opencode serve` on the
// configured address.
type Adapter struct {
	baseURL string

	mu     sync.Mutex
	client *Client
}

// New returns an adapter talking to an OpenCode server at baseURL.
func New(baseURL string) *Adapter { return &Adapter{baseURL: baseURL} }

var (
	_ adapter.Adapter       = (*Adapter)(nil)
	_ adapter.Discoverer    = (*Adapter)(nil)
	_ adapter.Transcriber   = (*Adapter)(nil)
	_ adapter.StateInferrer = (*Adapter)(nil)
	_ adapter.Approver      = (*Adapter)(nil)
	_ adapter.Locator       = (*Adapter)(nil)
)

func (*Adapter) Info() adapter.Info {
	return adapter.Info{
		ID:           "opencode",
		DisplayName:  "OpenCode",
		MinVersion:   MinVersion,
		NewestTested: "",
		Signals: adapter.Signals{
			Discovery: adapter.SupportExact,
			// Inferred from the database: how the last assistant message
			// ended, and whether a tool is still running. The server's SSE
			// stream would make it exact, but is out of reach for a TUI.
			State:      adapter.SupportDerived,
			Transcript: adapter.SupportExact,
			// Reply can answer a permission over REST, but a request is only
			// ever announced on the SSE stream, which nothing follows yet; the
			// database never records a pending one.
			Approvals: adapter.SupportNone,
		},
		Attachments: []protocol.Attachment{
			protocol.AttachNative,
			protocol.AttachWrap, protocol.AttachTmux, protocol.AttachTermctl,
		},
	}
}

func (*Adapter) Detect(p adapter.Process) bool {
	return len(p.Argv) > 0 && filepath.Base(p.Argv[0]) == "opencode"
}

func (a *Adapter) c() *Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil {
		a.client = NewClient(a.baseURL)
	}
	return a.client
}

// Discover lists sessions from OpenCode's database.
//
// No database is not an error: OpenCode is not installed, or predates 1.x.
func (a *Adapter) Discover(ctx context.Context) ([]adapter.Discovered, error) {
	db, err := openDB()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer db.Close()
	sessions, err := storedSessions(db, discoverLimit)
	if err != nil {
		return nil, err
	}
	out := make([]adapter.Discovered, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, adapter.Discovered{
			NaturalKey: s.ID,
			Cwd:        s.Directory,
			Title:      s.Title,
			Model:      s.Model,
			Ref:        s.ID,
			// No lease or exit record: live while the process is (the scan)
			// or, without one, while the session was used recently.
			Live: time.Since(s.Updated) < liveWindow,
		})
	}
	return out, nil
}

const (
	discoverLimit = 200
	liveWindow    = 12 * time.Hour
)

// OpenTranscript follows one session in the database. ref is the session id.
func (a *Adapter) OpenTranscript(ctx context.Context, ref string) (adapter.TranscriptStream, error) {
	db, err := openDB()
	if err != nil {
		return nil, err
	}
	return &stream{dbStream{db: db, sessionID: ref}}, nil
}

// stream adapts dbStream to the transcript interfaces.
type stream struct{ dbStream }

func (s *stream) Poll(context.Context) ([]protocol.TranscriptEvent, error) { return s.dbStream.Poll() }

var (
	_ adapter.SeekableStream = (*stream)(nil)
	_ adapter.EventReporter  = (*stream)(nil)
)

// InferState takes the server's event name when one came from the SSE
// stream, and otherwise the lifecycle the database last recorded.
func (*Adapter) InferState(in adapter.StateInput) (protocol.StateGuess, bool) {
	if strings.HasPrefix(in.Event, "db:") {
		return stateFromLifecycle(in.Event, in.Idle, in.Quiescence)
	}
	if in.Event != "" {
		return StateFromEvent(in.Event)
	}
	return protocol.StateGuess{}, false
}

// TranscriptForProcess has nothing to read: OpenCode records no per-process
// or per-terminal pointer to its session.
func (*Adapter) TranscriptForProcess(adapter.Process) string { return "" }

// MatchTranscript joins a running opencode to its session: one created in the
// same directory as the process started, else the only candidate, else the
// one written since the process began. The returned ref is a session id.
func (*Adapter) MatchTranscript(cwd string, started time.Time, claimed map[string]bool) string {
	if cwd == "" {
		return ""
	}
	db, err := openDB()
	if err != nil {
		return ""
	}
	defer db.Close()
	sessions, err := storedSessions(db, discoverLimit)
	if err != nil {
		return ""
	}
	var cands []StoredSession
	for _, s := range sessions {
		if !claimed[s.ID] && filepath.Clean(s.Directory) == filepath.Clean(cwd) {
			cands = append(cands, s)
		}
	}
	if len(cands) == 0 {
		return ""
	}
	if !started.IsZero() {
		for _, c := range cands {
			if d := c.Created.Sub(started); d > -time.Minute && d < time.Minute {
				return c.ID
			}
		}
	}
	if len(cands) == 1 {
		return cands[0].ID
	}
	if started.IsZero() {
		return ""
	}
	// cands is newest-updated first: the first written since the process
	// began is the one it is working in.
	for _, c := range cands {
		if !c.Updated.Before(started.Add(-5 * time.Second)) {
			return c.ID
		}
	}
	return ""
}

// Reply carries a human's decision back to OpenCode.
//
// ReplyFor maps the decision to OpenCode's vocabulary and deliberately never
// produces `always`, which would persist a broadened permission beyond the
// question that was asked. A person answering "yes" on their phone is
// answering this request, not granting a standing grant they cannot see.
func (a *Adapter) Reply(ctx context.Context, req protocol.ApprovalRequest, d protocol.ApprovalDecision) error {
	return a.c().ReplyPermission(ctx, req.SessionID, req.ID, ReplyFor(d), d.Reason)
}
