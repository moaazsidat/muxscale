package copilot

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/adapter/emit"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Adapter is the GitHub Copilot CLI integration: discovery, transcript and
// state from its session event logs. Copilot CLI has hooks (.github/hooks,
// sessionStart/agentStop and others) that could make state exact and carry
// approvals; installing them waits for a real install to test against.
type Adapter struct{}

func New() *Adapter { return &Adapter{} }

var (
	_ adapter.Adapter       = (*Adapter)(nil)
	_ adapter.Discoverer    = (*Adapter)(nil)
	_ adapter.Transcriber   = (*Adapter)(nil)
	_ adapter.StateInferrer = (*Adapter)(nil)
	_ adapter.Locator       = (*Adapter)(nil)
)

func (*Adapter) Info() adapter.Info {
	return adapter.Info{
		ID:          "copilot",
		DisplayName: "GitHub Copilot CLI",
		MinVersion:  MinVersion,
		Signals: adapter.Signals{
			Discovery: adapter.SupportExact,
			// Derived from persisted events, including permission requests:
			// closer to a report than a guess, but not the agent telling us.
			State:      adapter.SupportDerived,
			Transcript: adapter.SupportExact,
			Approvals:  adapter.SupportNone,
		},
		Attachments: []protocol.Attachment{
			protocol.AttachWrap, protocol.AttachTmux, protocol.AttachTermctl,
		},
	}
}

// Detect matches the `copilot` command and the npm package's script.
func (*Adapter) Detect(p adapter.Process) bool {
	if len(p.Argv) == 0 {
		return false
	}
	if filepath.Base(p.Argv[0]) == "copilot" {
		return true
	}
	for _, a := range p.Argv[1:] {
		if strings.Contains(a, "@github/copilot") {
			return true
		}
	}
	return false
}

// Discover lists ~/.copilot/session-state/*/events.jsonl. Only sessions
// written recently are read: an older one cannot be live, and reading every
// log this machine ever kept on each scan would cost for nothing.
func (*Adapter) Discover(context.Context) ([]adapter.Discovered, error) {
	root, err := sessionsRoot()
	if err != nil {
		return nil, err
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []adapter.Discovered
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		path := filepath.Join(root, d.Name(), "events.jsonl")
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		disc := adapter.Discovered{NaturalKey: d.Name(), Ref: path}
		if time.Since(fi.ModTime()) < liveWindow {
			if s, err := parseFile(path); err == nil {
				disc.NaturalKey = firstNonEmpty(s.ID, d.Name())
				disc.Cwd, disc.Title, disc.Model = s.Cwd, s.Title, s.Model
				disc.Live = s.life != lifeShutdown
			}
		}
		out = append(out, disc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

const liveWindow = 12 * time.Hour

func parseFile(path string) (*session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parse(f)
}

func (*Adapter) OpenTranscript(_ context.Context, ref string) (adapter.TranscriptStream, error) {
	return &stream{path: ref, emit: emit.New()}, nil
}

type stream struct {
	path string
	emit *emit.Emitter
	size int64
	life string
}

func (s *stream) Poll(context.Context) ([]protocol.TranscriptEvent, error) {
	fi, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if fi.Size() == s.size {
		return nil, nil // append-only: no growth, nothing new
	}
	sess, err := parseFile(s.path)
	if err != nil {
		return nil, err
	}
	s.size, s.life = fi.Size(), sess.life
	return s.emit.Next(sess.units), nil
}

func (s *stream) SeekEnd(fromSeq uint64) error { s.emit.SeekEnd(fromSeq); return nil }
func (s *stream) LastEvent() string            { return s.life }
func (s *stream) Close() error                 { return nil }

var (
	_ adapter.SeekableStream = (*stream)(nil)
	_ adapter.EventReporter  = (*stream)(nil)
)

func (*Adapter) InferState(in adapter.StateInput) (protocol.StateGuess, bool) {
	return stateFromLifecycle(in.Event, in.Idle, in.Quiescence)
}

func (*Adapter) TranscriptForProcess(adapter.Process) string { return "" }

// MatchTranscript joins a copilot process to the session it started in cwd.
func (*Adapter) MatchTranscript(cwd string, started time.Time, claimed map[string]bool) string {
	root, err := sessionsRoot()
	if err != nil || cwd == "" {
		return ""
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	type cand struct {
		path    string
		started time.Time
		mod     time.Time
	}
	var cands []cand
	for _, d := range dirs {
		path := filepath.Join(root, d.Name(), "events.jsonl")
		fi, err := os.Stat(path)
		if err != nil || claimed[path] || time.Since(fi.ModTime()) > liveWindow {
			continue
		}
		s, err := parseFile(path)
		if err != nil || filepath.Clean(s.Cwd) != filepath.Clean(cwd) || s.life == lifeShutdown {
			continue
		}
		cands = append(cands, cand{path, s.Started, fi.ModTime()})
	}
	if len(cands) == 0 {
		return ""
	}
	if !started.IsZero() {
		for _, c := range cands {
			if d := c.started.Sub(started); !c.started.IsZero() && d > -time.Minute && d < time.Minute {
				return c.path
			}
		}
	}
	if len(cands) == 1 {
		return cands[0].path
	}
	if started.IsZero() {
		return ""
	}
	var pick string
	var pickMod time.Time
	for _, c := range cands {
		if c.mod.Before(started.Add(-5*time.Second)) || !c.mod.After(pickMod) {
			continue
		}
		pick, pickMod = c.path, c.mod
	}
	return pick
}
