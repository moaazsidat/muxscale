package gemini

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

// Adapter is the Gemini CLI integration: discovery, transcript and inferred
// state from its session logs. Gemini CLI has hooks (AfterAgent, Notification)
// that would make state exact; installing them is not done yet, because a hook
// written into an agent's settings without a real install to test against is
// a hook that might break it.
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
		ID:          "gemini-cli",
		DisplayName: "Gemini CLI",
		MinVersion:  MinVersion,
		Signals: adapter.Signals{
			Discovery:  adapter.SupportExact,
			State:      adapter.SupportDerived,
			Transcript: adapter.SupportExact,
			Approvals:  adapter.SupportNone,
		},
		Attachments: []protocol.Attachment{
			protocol.AttachWrap, protocol.AttachTmux, protocol.AttachTermctl,
		},
	}
}

// Detect matches the `gemini` launcher and the npm package's script.
func (*Adapter) Detect(p adapter.Process) bool {
	if len(p.Argv) == 0 {
		return false
	}
	if filepath.Base(p.Argv[0]) == "gemini" {
		return true
	}
	for _, a := range p.Argv[1:] {
		if strings.Contains(a, "@google/gemini-cli") {
			return true
		}
	}
	return false
}

// Discover walks ~/.gemini/tmp/*/chats.
func (*Adapter) Discover(context.Context) ([]adapter.Discovered, error) {
	root, err := geminiDir()
	if err != nil {
		return nil, err
	}
	projects, err := os.ReadDir(filepath.Join(root, "tmp"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	cwds := projectDirs(root)
	var out []adapter.Discovered
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(root, "tmp", p.Name(), "chats")
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !isSessionFile(f.Name()) {
				continue // a directory here holds a session's subagents
			}
			path := filepath.Join(dir, f.Name())
			info, err := f.Info()
			if err != nil {
				continue
			}
			h, ok := readHead(path)
			if !ok || h.kind == "subagent" {
				continue
			}
			out = append(out, adapter.Discovered{
				NaturalKey: h.id,
				Cwd:        cwds[p.Name()],
				Title:      h.title,
				Ref:        path,
				Live:       time.Since(info.ModTime()) < liveWindow,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// liveWindow: no lease and no exit record, so live while the process is, or
// while the log was written recently. A session waiting on its person is
// quiet by definition, hence generous.
const liveWindow = 12 * time.Hour

func isSessionFile(name string) bool {
	return strings.HasPrefix(name, "session-") && (strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".json"))
}

type head struct {
	id, title, kind string
	started         time.Time
}

// readHead replays a session for its id and a label. Sessions are capped at
// 30 days by default, so this stays proportionate; a summary, when Gemini has
// written one, beats the first prompt.
func readHead(path string) (head, bool) {
	f, err := os.Open(path)
	if err != nil {
		return head{}, false
	}
	defer f.Close()
	c, err := replay(f)
	if err != nil || c.SessionID == "" {
		return head{}, false
	}
	h := head{id: c.SessionID, title: c.Summary, kind: c.Kind, started: c.StartTime}
	if h.title == "" {
		for _, m := range c.messages {
			if m.Type == "user" {
				h.title = oneLine(firstNonEmpty(partsText(m.Display), partsText(m.Content)), 80)
				break
			}
		}
	}
	return h, true
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func (*Adapter) OpenTranscript(_ context.Context, ref string) (adapter.TranscriptStream, error) {
	return &stream{path: ref, emit: emit.New()}, nil
}

// stream re-reads the log on each poll; a log is replayed, never tailed,
// because a later line can rewrite or remove an earlier message.
type stream struct {
	path    string
	emit    *emit.Emitter
	size    int64
	modTime time.Time
	life    string
}

func (s *stream) Poll(context.Context) ([]protocol.TranscriptEvent, error) {
	fi, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if fi.Size() == s.size && fi.ModTime().Equal(s.modTime) {
		return nil, nil
	}
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c, err := replay(f)
	if err != nil {
		return nil, err
	}
	s.size, s.modTime = fi.Size(), fi.ModTime()
	s.life = c.lifecycle()
	return s.emit.Next(c.units()), nil
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

// TranscriptForProcess: Gemini CLI records no per-process pointer to its log.
func (*Adapter) TranscriptForProcess(adapter.Process) string { return "" }

// MatchTranscript finds the session a gemini process started in cwd is
// writing: the one that began with it, else the only one, else the newest
// written since the process began.
func (*Adapter) MatchTranscript(cwd string, started time.Time, claimed map[string]bool) string {
	if cwd == "" {
		return ""
	}
	root, err := geminiDir()
	if err != nil {
		return ""
	}
	type cand struct {
		path    string
		started time.Time
		mod     time.Time
	}
	var cands []cand
	for _, id := range projectIDs(root, cwd) {
		dir := filepath.Join(root, "tmp", id, "chats")
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			path := filepath.Join(dir, f.Name())
			if f.IsDir() || !isSessionFile(f.Name()) || claimed[path] {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			h, ok := readHead(path)
			if !ok || h.kind == "subagent" {
				continue
			}
			cands = append(cands, cand{path, h.started, info.ModTime()})
		}
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
