package kimi

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/adapter/emit"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Adapter is the Kimi Code CLI integration: discovery, transcript and state
// from its session wire files. Kimi persists approval requests, so a pending
// one is read from the file; answering it remotely would need Kimi's own
// protocol (ACP), which is not wired up.
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
		ID:          "kimi",
		DisplayName: "Kimi Code",
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

// Detect matches the `kimi` command and the npm package's script.
func (*Adapter) Detect(p adapter.Process) bool {
	if len(p.Argv) == 0 {
		return false
	}
	if b := filepath.Base(p.Argv[0]); b == "kimi" || b == "kimi-code" {
		return true
	}
	for _, a := range p.Argv[1:] {
		if strings.Contains(a, "@moonshot-ai/kimi-code") {
			return true
		}
	}
	return false
}

// indexed is one line of session_index.jsonl.
type indexed struct {
	SessionID  string `json:"sessionId"`
	SessionDir string `json:"sessionDir"`
	WorkDir    string `json:"workDir"`
}

// state is the part of state.json discovery uses.
type state struct {
	Title      string `json:"title"`
	LastPrompt string `json:"lastPrompt"`
	CreatedAt  any    `json:"createdAt"`
	UpdatedAt  any    `json:"updatedAt"`
}

// sessions lists every session: the index first, and any directory it does
// not mention (an index rebuilt late, a session imported by hand) after.
func sessions() ([]indexed, error) {
	root, err := home()
	if err != nil {
		return nil, err
	}
	var out []indexed
	seen := map[string]bool{}
	if f, err := os.Open(filepath.Join(root, "session_index.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var e indexed
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.SessionID != "" && e.SessionDir != "" && !seen[e.SessionID] {
				seen[e.SessionID] = true
				out = append(out, e)
			}
		}
		f.Close()
	}
	buckets, err := os.ReadDir(filepath.Join(root, "sessions"))
	if err != nil {
		if os.IsNotExist(err) && len(out) == 0 {
			return nil, nil
		}
		return out, nil
	}
	for _, b := range buckets {
		if !b.IsDir() {
			continue
		}
		ids, err := os.ReadDir(filepath.Join(root, "sessions", b.Name()))
		if err != nil {
			continue
		}
		for _, id := range ids {
			if id.IsDir() && !seen[id.Name()] {
				seen[id.Name()] = true
				out = append(out, indexed{SessionID: id.Name(), SessionDir: filepath.Join(root, "sessions", b.Name(), id.Name())})
			}
		}
	}
	return out, nil
}

func readState(dir string) state {
	var st state
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	return st
}

func wirePath(sessionDir string) string {
	return filepath.Join(sessionDir, "agents", "main", "wire.jsonl")
}

func (*Adapter) Discover(context.Context) ([]adapter.Discovered, error) {
	list, err := sessions()
	if err != nil {
		return nil, err
	}
	var out []adapter.Discovered
	for _, e := range list {
		path := wirePath(e.SessionDir)
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		st := readState(e.SessionDir)
		out = append(out, adapter.Discovered{
			NaturalKey: e.SessionID,
			Cwd:        e.WorkDir,
			Title:      firstNonEmpty(st.Title, oneLine(st.LastPrompt, 80)),
			Ref:        path,
			Live:       time.Since(fi.ModTime()) < liveWindow,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

const liveWindow = 12 * time.Hour

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
	sess, err := parse(f)
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

// MatchTranscript joins a kimi process to a session in its working directory's
// bucket: the one created as it started, else the only one, else the newest
// written since.
func (*Adapter) MatchTranscript(cwd string, started time.Time, claimed map[string]bool) string {
	root, err := home()
	if err != nil || cwd == "" {
		return ""
	}
	bucket := filepath.Join(root, "sessions", workDirKey(filepath.Clean(cwd)))
	ids, err := os.ReadDir(bucket)
	if err != nil {
		return ""
	}
	type cand struct {
		path    string
		created time.Time
		mod     time.Time
	}
	var cands []cand
	for _, id := range ids {
		dir := filepath.Join(bucket, id.Name())
		path := wirePath(dir)
		fi, err := os.Stat(path)
		if !id.IsDir() || err != nil || claimed[path] {
			continue
		}
		cands = append(cands, cand{path, stamp(readState(dir).CreatedAt), fi.ModTime()})
	}
	if len(cands) == 0 {
		return ""
	}
	if !started.IsZero() {
		for _, c := range cands {
			if d := c.created.Sub(started); !c.created.IsZero() && d > -time.Minute && d < time.Minute {
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

// stamp reads a state.json timestamp: an ISO string or epoch milliseconds.
func stamp(v any) time.Time {
	switch x := v.(type) {
	case string:
		t, _ := time.Parse(time.RFC3339Nano, x)
		return t
	case float64:
		return time.UnixMilli(int64(x))
	}
	return time.Time{}
}
