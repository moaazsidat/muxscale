package pi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/adapter/omp"
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
	_ adapter.Locator       = (*Adapter)(nil)
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

// Layout is where pi keeps sessions: PI_CODING_AGENT_DIR, else ~/.pi/agent,
// one --<cwd with slashes as hyphens>-- directory per project.
//
// The format is the one omp inherited from pi -- verified against pi 0.83's
// docs/session-format.md -- so pi reads its files with the same parser and
// joins processes to them with the same rules. pi holds no writer lease, so a
// session is live while its process is (the process scan) or, without one,
// while its file has been written recently.
var Layout = omp.Layout{
	Agent: "pi",
	AgentDir: func() (string, error) {
		if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
			return d, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".pi", "agent"), nil
	},
	Bucket: func(cwd string) (string, error) {
		if cwd == "" {
			return "", fmt.Errorf("no working directory")
		}
		return SessionSlug(cwd), nil
	},
}

// Discover walks pi's session directories.
func (*Adapter) Discover(context.Context) ([]adapter.Discovered, error) {
	return Layout.Discover()
}

func (*Adapter) OpenTranscript(_ context.Context, ref string) (adapter.TranscriptStream, error) {
	return omp.NewStream(ref), nil
}

// TranscriptForProcess has nothing to read: pi leaves no per-terminal record
// of which session it is showing. MatchTranscript joins by start time instead.
func (*Adapter) TranscriptForProcess(adapter.Process) string { return "" }

func (*Adapter) MatchTranscript(cwd string, started time.Time, claimed map[string]bool) string {
	return Layout.MatchTranscript(cwd, started, claimed)
}

// InferState takes pi's own event name when muxscale owns the session's pipe,
// and otherwise reads the transcript, as for omp.
func (*Adapter) InferState(in adapter.StateInput) (protocol.StateGuess, bool) {
	if in.Event != "" {
		return InferState(in.Event)
	}
	return omp.InferTreeState(in, "pi")
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
