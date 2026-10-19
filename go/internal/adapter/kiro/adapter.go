package kiro

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/adapter/emit"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Adapter is the Kiro CLI integration: discovery and the conversation's text
// from its session files, with state inferred from who spoke last. Kiro's
// agent hooks (agentSpawn, userPromptSubmit, preToolUse, postToolUse, stop)
// would make state exact; installing them waits for a real install.
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
		ID:          "kiro",
		DisplayName: "Kiro CLI",
		MinVersion:  MinVersion,
		Signals: adapter.Signals{
			Discovery: adapter.SupportExact,
			State:     adapter.SupportDerived,
			// Derived: prompts and replies only, without tool calls.
			Transcript: adapter.SupportDerived,
			Approvals:  adapter.SupportNone,
		},
		Attachments: []protocol.Attachment{
			protocol.AttachWrap, protocol.AttachTmux, protocol.AttachTermctl,
		},
	}
}

// Detect matches Kiro's CLI binaries. `kiro` alone is the IDE's launcher.
func (*Adapter) Detect(p adapter.Process) bool {
	if len(p.Argv) == 0 {
		return false
	}
	switch filepath.Base(p.Argv[0]) {
	case "kiro-cli", "kiro-cli-chat":
		return true
	}
	return false
}

func (*Adapter) Discover(context.Context) ([]adapter.Discovered, error) {
	sessions, err := list()
	if err != nil {
		return nil, err
	}
	out := make([]adapter.Discovered, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, adapter.Discovered{
			NaturalKey: s.meta.id(),
			Cwd:        s.meta.cwd(),
			Title:      s.meta.Title,
			Ref:        s.messages,
			Live:       !s.mod.IsZero() && time.Since(s.mod) < liveWindow,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

const liveWindow = 12 * time.Hour

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
		return nil, nil
	}
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	units, life, err := parse(f)
	if err != nil {
		return nil, err
	}
	s.size, s.life = fi.Size(), life
	return s.emit.Next(units), nil
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

// MatchTranscript joins a kiro-cli process to a session in its directory.
func (*Adapter) MatchTranscript(cwd string, started time.Time, claimed map[string]bool) string {
	if cwd == "" {
		return ""
	}
	sessions, err := list()
	if err != nil {
		return ""
	}
	var cands []found
	for _, s := range sessions {
		if !claimed[s.messages] && filepath.Clean(s.meta.cwd()) == filepath.Clean(cwd) {
			cands = append(cands, s)
		}
	}
	if len(cands) == 0 {
		return ""
	}
	if !started.IsZero() {
		for _, c := range cands {
			if d := c.meta.started().Sub(started); !c.meta.started().IsZero() && d > -time.Minute && d < time.Minute {
				return c.messages
			}
		}
	}
	if len(cands) == 1 {
		return cands[0].messages
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
		pick, pickMod = c.messages, c.mod
	}
	return pick
}
