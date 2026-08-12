package opencode

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Adapter is the OpenCode integration.
//
// The only adapter besides Claude Code with a genuine native channel: OpenCode
// runs a local HTTP server with an SSE event stream and REST endpoints for
// prompting, answering permissions, and interrupting. That means remote input
// works without owning a PTY, which is why AttachNative leads its list.
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
)

func (*Adapter) Info() adapter.Info {
	return adapter.Info{
		ID:           "opencode",
		DisplayName:  "OpenCode",
		MinVersion:   MinVersion,
		NewestTested: "",
		Signals: adapter.Signals{
			Discovery: adapter.SupportExact,
			// The SSE stream carries the agent's own event names, including
			// permission.asked, so state is authoritative while connected.
			State:      adapter.SupportExact,
			Transcript: adapter.SupportExact,
			// permission.asked plus a REST reply endpoint is a real decision
			// channel: the answer is structured data, not keystrokes.
			Approvals: adapter.SupportExact,
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

// Discover asks the OpenCode server for its sessions.
//
// A server that is not running is not an error: OpenCode simply is not up, and
// a failed dial here would otherwise surface to the user as a broken daemon
// rather than an absent agent.
func (a *Adapter) Discover(ctx context.Context) ([]adapter.Discovered, error) {
	if err := a.c().Health(ctx); err != nil {
		return nil, nil
	}
	sessions, err := a.c().Sessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]adapter.Discovered, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, adapter.Discovered{
			NaturalKey: s.ID,
			// Cwd() prefers the undocumented `location` object over the
			// documented-but-null `directory` field. See opencode.go.
			Cwd:   s.Cwd(),
			Title: s.Title,
			Model: s.ModelName(),
			Ref:   s.ID,
			Live:  true,
		})
	}
	return out, nil
}

// OpenTranscript subscribes to the event stream and filters it to one session.
//
// Unlike every other adapter, OpenCode has no file to re-read: the transcript
// exists only as it streams past. So this buffers events as they arrive and
// hands them over on Poll, which means a session's history *before* muxscale
// connected is not available -- an honest limitation of a live-only source,
// recorded here rather than papered over with an empty transcript.
func (a *Adapter) OpenTranscript(ctx context.Context, ref string) (adapter.TranscriptStream, error) {
	ctx, cancel := context.WithCancel(ctx)
	s := &stream{sessionID: ref, cancel: cancel}

	go func() {
		// A dropped stream is not fatal: OpenCode may be restarting. Poll keeps
		// returning what was already buffered and the daemon's own reconnect
		// logic decides whether to reopen.
		_ = StreamEvents(ctx, a.baseURL, func(e Event) {
			if e.SessionID() != s.sessionID {
				return
			}
			if evs := ToTranscript(e); len(evs) > 0 {
				s.mu.Lock()
				s.buf = append(s.buf, evs...)
				s.mu.Unlock()
			}
		})
	}()
	return s, nil
}

type stream struct {
	sessionID string
	cancel    context.CancelFunc

	mu  sync.Mutex
	buf []protocol.TranscriptEvent
}

func (s *stream) Poll(context.Context) ([]protocol.TranscriptEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) == 0 {
		return nil, nil
	}
	out := s.buf
	// Release the buffer rather than reslicing, so a burst does not pin its
	// backing array for the life of the session.
	s.buf = nil
	return out, nil
}

func (s *stream) Close() error {
	s.cancel()
	return nil
}

func (*Adapter) InferState(in adapter.StateInput) (protocol.StateGuess, bool) {
	if in.Event == "" {
		return protocol.StateGuess{}, false
	}
	return StateFromEvent(in.Event)
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
