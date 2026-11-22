package antigravity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Adapter is the Google Antigravity CLI (`agy`) integration.
type Adapter struct{}

// New returns the Antigravity CLI adapter.
func New() *Adapter { return &Adapter{} }

var (
	_ adapter.Adapter       = (*Adapter)(nil)
	_ adapter.Discoverer    = (*Adapter)(nil)
	_ adapter.Transcriber   = (*Adapter)(nil)
	_ adapter.StateInferrer = (*Adapter)(nil)
	_ adapter.Locator       = (*Adapter)(nil)
	_ adapter.Liveness      = (*Adapter)(nil)
	_ adapter.HookCodec     = (*Adapter)(nil)
	_ adapter.Installer     = (*Adapter)(nil)
	_ adapter.KeyApprover   = (*Adapter)(nil)
	_ adapter.KeyStepper    = (*Adapter)(nil)
)

func (*Adapter) Info() adapter.Info {
	return adapter.Info{
		ID:           "antigravity",
		DisplayName:  "Antigravity CLI",
		MinVersion:   MinVersion,
		NewestTested: NewestTested,
		Signals: adapter.Signals{
			Discovery: adapter.SupportExact,
			// Hooks (SessionStart, PreInvocation, Stop) report exact turn
			// boundaries; without them the transcript and presence lock give
			// inferred state.
			State:      adapter.SupportExact,
			Transcript: adapter.SupportExact,
			// Blocking permission prompts and ask_question dialogs are read
			// from the conversation database's waiting step and answered by
			// typing Antigravity's own keys into the terminal (KeyStepper).
			Approvals: adapter.SupportDerived,
		},
		Attachments: []protocol.Attachment{
			protocol.AttachWrap, protocol.AttachTmux, protocol.AttachTermctl,
		},
	}
}

// Detect matches the `agy` and `antigravity-cli` binaries.
func (*Adapter) Detect(p adapter.Process) bool {
	if len(p.Argv) == 0 {
		return false
	}
	base := strings.TrimSuffix(filepath.Base(p.Argv[0]), ".exe")
	return base == "agy" || base == "antigravity-cli"
}

// Discover enumerates top-level Antigravity CLI conversations from
// conversation_summaries.db, falling back to brain/ when the database is absent.
func (*Adapter) Discover(context.Context) ([]adapter.Discovered, error) {
	root, err := DataDir()
	if err != nil {
		return nil, err
	}
	rows, err := readSummaries(root, discoverLimit)
	if err != nil {
		if os.IsNotExist(err) {
			return discoverFromBrain(root)
		}
		// If the DB exists but cannot be queried (e.g. schema drift), degrade
		// to scanning brain/ rather than breaking discovery.
		if fb, fbErr := discoverFromBrain(root); fbErr == nil {
			return fb, nil
		}
		return nil, err
	}
	out := make([]adapter.Discovered, 0, len(rows))
	for _, r := range rows {
		ref := TranscriptPath(root, r.ID)
		live, known := liveByID(root, r.ID)
		if !known {
			live = !r.LastModified.IsZero() && time.Since(r.LastModified) < liveWindow
		}
		title := strings.TrimSpace(r.Title)
		if title == "" {
			title = firstPromptFromFile(ref)
		}
		out = append(out, adapter.Discovered{
			NaturalKey: r.ID,
			Cwd:        r.Cwd,
			Title:      title,
			Ref:        ref,
			Live:       live,
		})
	}
	return out, nil
}

// OpenTranscript tails one conversation's transcript_full.jsonl.
func (*Adapter) OpenTranscript(_ context.Context, ref string) (adapter.TranscriptStream, error) {
	return newStream(ref), nil
}

// Live checks the conversation's presence flock.
func (*Adapter) Live(ref string) (bool, bool) { return Live(ref) }

// InferState derives session state from the presence lock and transcript steps.
func (*Adapter) InferState(in adapter.StateInput) (protocol.StateGuess, bool) {
	return stateFromLifecycle(in)
}

// TranscriptForProcess: Antigravity records no per-PID pointer on disk; hook
// events carry X-Muxscale-Peer-PID to link a running process directly.
func (*Adapter) TranscriptForProcess(adapter.Process) string { return "" }

// MatchTranscript joins a running `agy` process to an unclaimed conversation in
// cwd, preferring one whose presence flock is currently held.
func (*Adapter) MatchTranscript(cwd string, started time.Time, claimed map[string]bool) string {
	if cwd == "" {
		return ""
	}
	root, err := DataDir()
	if err != nil {
		return ""
	}
	rows, err := readSummaries(root, discoverLimit)
	if err != nil {
		return ""
	}
	cleanCwd := canonical(cwd)
	type cand struct {
		ref  string
		live bool
		mod  time.Time
		in   time.Time
	}
	var cands []cand
	for _, r := range rows {
		if r.Cwd == "" || canonical(r.Cwd) != cleanCwd {
			continue
		}
		ref := TranscriptPath(root, r.ID)
		if claimed[ref] {
			continue
		}
		live, known := liveByID(root, r.ID)
		if !known {
			live = !r.LastModified.IsZero() && time.Since(r.LastModified) < liveWindow
		}
		cands = append(cands, cand{ref: ref, live: live, mod: r.LastModified, in: r.LastInput})
	}
	if len(cands) == 0 {
		return ""
	}
	// Prefer live (flock-held) conversations first.
	var liveCands []cand
	for _, c := range cands {
		if c.live {
			liveCands = append(liveCands, c)
		}
	}
	if len(liveCands) == 1 {
		return liveCands[0].ref
	}
	pool := cands
	if len(liveCands) > 0 {
		pool = liveCands
	}
	if !started.IsZero() {
		for _, c := range pool {
			t := c.in
			if t.IsZero() {
				t = c.mod
			}
			if d := t.Sub(started); !t.IsZero() && d > -time.Minute && d < time.Minute {
				return c.ref
			}
		}
	}
	if len(pool) == 1 {
		return pool[0].ref
	}
	if started.IsZero() {
		return ""
	}
	// rows are ordered newest last_modified_time first.
	for _, c := range pool {
		if !c.mod.IsZero() && !c.mod.Before(started.Add(-5*time.Second)) {
			return c.ref
		}
	}
	return ""
}

func canonical(p string) string {
	if r, err := filepath.EvalSymlinks(filepath.Clean(p)); err == nil {
		return r
	}
	return filepath.Clean(p)
}
