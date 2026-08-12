package pi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Adapter is the pi integration (Earendil Works).
//
// Spec §4.5.7 promotes pi to the reference native integration, and M3.5 uses it
// as the test of whether this seam is real: pi is meant to ship without
// changing the protocol, the daemon core, or the app. Where that failed, the
// seam was widened rather than pi special-cased -- see StateInput.Ended, which
// exists because hermes needed it and pi's own state path did not.
type Adapter struct {
	// live maps a session to the command pipe muxscale owns for it.
	//
	// pi's approval channel is real but not ambient: BuildUIResponse writes to
	// the extension pipe, which exists only for sessions muxscale launched or
	// wrapped. So Reply is answerable per session rather than per adapter, and
	// a session with no entry here gets an explicit error instead of a silently
	// dropped decision.
	mu   sync.Mutex
	live map[string]*Client

	// asked retains the original UIRequest for each outstanding approval.
	//
	// BuildUIResponse branches on Method -- a confirm is answered with a
	// boolean, a select with a value -- and protocol.ApprovalRequest does not
	// carry it, deliberately: pi's dialog vocabulary has no business in a type
	// the app and the Rust core also compile against. Keeping it on this side
	// of the seam is the cost of that, and it is the right side to pay it on.
	asked map[string]UIRequest
}

func New() *Adapter {
	return &Adapter{
		live:  map[string]*Client{},
		asked: map[string]UIRequest{},
	}
}

// RecordRequest retains a UI request so the eventual answer can be shaped
// correctly. Called by whatever is reading the session's event stream.
func (a *Adapter) RecordRequest(r UIRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked[r.ID] = r
}

// Attach registers the command pipe for a session muxscale owns. The wrap
// attachment calls this once the extension handshake completes.
func (a *Adapter) Attach(sessionID string, c *Client) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.live[sessionID] = c
}

// Detach drops a session's pipe when it exits.
func (a *Adapter) Detach(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.live, sessionID)
}

// take returns and removes a retained request, so answering twice is not
// possible and the map cannot grow without bound.
func (a *Adapter) take(id string) (UIRequest, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.asked[id]
	delete(a.asked, id)
	return r, ok
}

func (a *Adapter) clientFor(sessionID string) (*Client, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.live[sessionID]
	return c, ok
}

var (
	_ adapter.Adapter       = (*Adapter)(nil)
	_ adapter.Discoverer    = (*Adapter)(nil)
	_ adapter.Transcriber   = (*Adapter)(nil)
	_ adapter.StateInferrer = (*Adapter)(nil)
	_ adapter.Approver      = (*Adapter)(nil)
)

func (*Adapter) Info() adapter.Info {
	return adapter.Info{
		ID:           "pi",
		DisplayName:  "pi",
		MinVersion:   MinVersion,
		NewestTested: "",
		Signals: adapter.Signals{
			Discovery: adapter.SupportExact,
			// pi's extension protocol names its states, so when muxscale owns
			// the pipe the state is the agent's own.
			State:      adapter.SupportExact,
			Transcript: adapter.SupportExact,
			// pi asks structured UI questions and waits for a structured
			// answer -- the same shape as Claude Code's PermissionRequest.
			Approvals: adapter.SupportExact,
		},
		// AttachNative is deliberately absent despite pi having a command
		// channel. Send works over a pipe muxscale owns; whether an extension
		// can originate a prompt into a session started independently is spec
		// §4.5.7's outstanding [VERIFY], and claiming native input before that
		// is answered would promise remote input that silently does nothing
		// for every unwrapped pi session.
		Attachments: []protocol.Attachment{
			protocol.AttachWrap, protocol.AttachTmux, protocol.AttachTermctl,
		},
	}
}

func (*Adapter) Detect(p adapter.Process) bool {
	return len(p.Argv) > 0 && filepath.Base(p.Argv[0]) == "pi"
}

// Discover walks pi's session directories.
//
// pi names each directory after the working directory it was started in, so
// unlike Codex the project is recoverable from the path alone, without opening
// every file.
func (*Adapter) Discover(context.Context) ([]adapter.Discovered, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, ".pi", "agent", "sessions")
	dirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // pi isn't installed.
		}
		return nil, err
	}

	var out []adapter.Discovered
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		cwd := cwdFromSlug(d.Name())
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(root, d.Name(), f.Name())
			info, statErr := f.Info()
			out = append(out, adapter.Discovered{
				NaturalKey: strings.TrimSuffix(f.Name(), ".jsonl"),
				Cwd:        cwd,
				Ref:        path,
				Live:       statErr == nil && time.Since(info.ModTime()) < liveWindow,
			})
		}
	}
	// Deterministic order; the filesystem gives none.
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// liveWindow matches the Codex adapter's reasoning: a session blocked on a
// human is idle by definition, so a short window would hide exactly the
// sessions worth surfacing.
const liveWindow = 12 * time.Hour

// cwdFromSlug inverts SessionSlug: --Users-sam-dev-x-- -> /Users/sam/dev/x
//
// Lossy, and knowingly so: a directory whose own name contains a hyphen is
// indistinguishable from a path separator once slugged. The result is used for
// display and grouping, never to resolve a path on disk, so a wrong guess
// mislabels a project rather than touching the wrong directory.
func cwdFromSlug(slug string) string {
	s := strings.Trim(slug, "-")
	if s == "" {
		return ""
	}
	return "/" + strings.ReplaceAll(s, "-", "/")
}

func (*Adapter) OpenTranscript(_ context.Context, ref string) (adapter.TranscriptStream, error) {
	return &stream{path: ref}, nil
}

// stream re-reads the session file and emits what is new, for the same reason
// the Codex stream does: pi entries are variable-length JSON lines and the
// final one is frequently half-written.
type stream struct {
	path    string
	emitted int
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

	events, _, err := ParseSession(f)
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

func (*Adapter) InferState(in adapter.StateInput) (protocol.StateGuess, bool) {
	if in.Event == "" {
		return protocol.StateGuess{}, false
	}
	return InferState(in.Event)
}

// Reply sends a human's decision back over pi's extension pipe.
//
// The error when no pipe exists is deliberate and must not become a silent
// success: a decision that goes nowhere leaves the person believing they
// unblocked their agent while it sits waiting. Better to fail loudly and let
// the UI say the session cannot be answered remotely.
func (a *Adapter) Reply(_ context.Context, req protocol.ApprovalRequest, d protocol.ApprovalDecision) error {
	c, ok := a.clientFor(req.SessionID)
	if !ok {
		return fmt.Errorf("pi session %s has no muxscale-owned command pipe, so "+
			"a decision cannot be delivered; it must be answered at the terminal",
			req.SessionID)
	}
	orig, ok := a.take(req.ID)
	if !ok {
		// Without the original request there is no way to know whether pi wants
		// a boolean or a string, and guessing wrong answers the question
		// incorrectly rather than not at all -- much worse than refusing.
		return fmt.Errorf("pi request %s is not outstanding; it may have already "+
			"been answered or timed out", req.ID)
	}
	return c.SendUIResponse(BuildUIResponse(orig, d))
}
