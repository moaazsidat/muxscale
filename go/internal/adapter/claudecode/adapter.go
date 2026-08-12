package claudecode

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
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Adapter is the Claude Code integration -- the litmus adapter (spec §4.5.4).
// It is the only one with a blocking approval channel, which is why it was
// built first: an interface designed against a read-only agent would not have
// room for the case that matters most.
type Adapter struct{}

// New returns the Claude Code adapter.
func New() *Adapter { return &Adapter{} }

var (
	_ adapter.Adapter       = (*Adapter)(nil)
	_ adapter.Discoverer    = (*Adapter)(nil)
	_ adapter.Transcriber   = (*Adapter)(nil)
	_ adapter.StateInferrer = (*Adapter)(nil)
	_ adapter.Installer     = (*Adapter)(nil)
)

func (*Adapter) Info() adapter.Info {
	return adapter.Info{
		ID:           "claude-code",
		DisplayName:  "Claude Code",
		MinVersion:   MinVersion,
		NewestTested: newestKnownProfile,
		Signals: adapter.Signals{
			// Transcript files exist for sessions that are not running, so
			// discovery does not depend on catching the process.
			Discovery: adapter.SupportExact,
			// Hooks report turn boundaries and permission requests
			// authoritatively, which is the whole reason this adapter leads.
			State:      adapter.SupportExact,
			Transcript: adapter.SupportExact,
			// PermissionRequest is held open server-side until a human answers,
			// so the reply is a structured decision rather than keystrokes.
			Approvals: adapter.SupportExact,
		},
		// Hooks carry approvals without a PTY, but free-text input still needs
		// somewhere to type it. Ordered best-first.
		Attachments: []protocol.Attachment{
			protocol.AttachWrap, protocol.AttachTmux, protocol.AttachTermctl,
		},
	}
}

// Detect matches the `claude` binary.
//
// Matching argv[0]'s base name rather than the resolved executable, because
// Claude Code is commonly installed as a shim, a bun/node wrapper, or a
// version-manager shell function, and the resolved path is then some
// node binary shared with every other tool on the machine.
func (*Adapter) Detect(p adapter.Process) bool {
	if len(p.Argv) == 0 {
		return false
	}
	if base(p.Argv[0]) == "claude" {
		return true
	}
	// Installed via npm, the process is `node .../@anthropic-ai/claude-code/cli.js`.
	for _, a := range p.Argv[1:] {
		if strings.Contains(a, "@anthropic-ai/claude-code") {
			return true
		}
	}
	return false
}

func base(s string) string {
	return strings.TrimSuffix(filepath.Base(s), ".exe")
}

// Discover walks ~/.claude/projects for session transcripts.
//
// This finds sessions that are not currently running, which the process scan
// cannot, and it is what makes a session that ended before muxscale started
// still appear with its full history.
func (*Adapter) Discover(context.Context) ([]adapter.Discovered, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	projects, err := os.ReadDir(filepath.Join(home, ".claude", "projects"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // Claude Code isn't installed, or has never run.
		}
		return nil, err
	}

	var out []adapter.Discovered
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(home, ".claude", "projects", p.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, f.Name())
			info, statErr := f.Info()
			out = append(out, adapter.Discovered{
				NaturalKey: strings.TrimSuffix(f.Name(), ".jsonl"),
				// Read from the file's first record rather than un-slugging the
				// directory name. The slug replaces '/' with '-' and is
				// therefore ambiguous for any path containing a hyphen -- which
				// most checkouts do.
				Cwd:  headerCwd(path),
				Ref:  path,
				Live: statErr == nil && time.Since(info.ModTime()) < liveWindow,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// liveWindow is how recently a transcript must have changed for the session to
// count as live. Generous on purpose: a session blocked on a human is idle by
// definition, and a tight window would hide exactly those.
const liveWindow = 12 * time.Hour

// headerCwd reads the cwd from a transcript's first record without parsing the
// whole file. Discovery runs over every session on the machine, so reading
// megabytes of conversation to recover one string is the difference between a
// scan that is free and one the user notices.
func headerCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for i := 0; i < headerScanLines && sc.Scan(); i++ {
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err == nil && rec.Cwd != "" {
			return rec.Cwd
		}
	}
	return ""
}

// headerScanLines bounds the search: the cwd appears on the first record in
// every transcript observed, and a few extra lines cover a leading summary or
// meta entry without turning this into a full parse.
const headerScanLines = 8

// OpenTranscript tails a session's JSONL file. ref is the transcript path.
func (*Adapter) OpenTranscript(_ context.Context, ref string) (adapter.TranscriptStream, error) {
	return &stream{t: NewTailer(ref, 750*time.Millisecond)}, nil
}

// stream adapts the existing Tailer, which already holds exactly the state the
// interface needs: a byte offset and the remainder of any partial final line.
type stream struct{ t *Tailer }

func (s *stream) Poll(context.Context) ([]protocol.TranscriptEvent, error) {
	ev, _, err := s.t.Poll()
	return ev, err
}

// Close is a no-op: the Tailer opens the file per poll rather than holding a
// descriptor, so a transcript can be rotated or deleted underneath it.
func (s *stream) Close() error { return nil }

// InferState derives state from the transcript, for sessions with no hooks --
// started before muxscale, or on a version below the floor. The hook path does
// not come through here: it produces an exact state directly, and routing it
// through inference would only be an opportunity to lose confidence.
func (*Adapter) InferState(in adapter.StateInput) (protocol.StateGuess, bool) {
	if len(in.Recent) == 0 {
		return protocol.StateGuess{}, false
	}
	return InferState(in.Recent, time.Now().Add(-in.Idle), in.Quiescence), true
}

func (*Adapter) PlanInstall(context.Context) (adapter.InstallPlan, error) {
	return BuildInstall(InstallOptions{})
}

func (*Adapter) PlanUninstall(context.Context) (adapter.InstallPlan, error) {
	path, err := DefaultSettingsPath()
	if err != nil {
		return nil, err
	}
	return BuildUninstall(path)
}

// Installed reports whether our hooks are in settings.json, computed by asking
// what an install would change: if it would add nothing, they are already there.
// Deriving it from the same code that performs the install keeps the two from
// disagreeing, which is how "already installed" claims go stale.
func (a *Adapter) Installed(ctx context.Context) (bool, error) {
	p, err := a.PlanInstall(ctx)
	if err != nil {
		return false, err
	}
	return p.Empty(), nil
}
