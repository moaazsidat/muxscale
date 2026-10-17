package claudecode

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Live transcript tailing.
//
// This is what makes a session muxscale never launched actually useful: the
// full structured transcript, reconstructed from the file Claude Code is
// already writing, including everything that happened before muxscale existed.
//
// Polling by size rather than fsnotify is deliberate. A JSONL file that is
// appended to constantly generates a torrent of write events, and coalescing
// them costs more than a cheap Stat at a modest interval. Polling also behaves
// identically on macOS and Linux, and degrades gracefully on network mounts.

// Tailer follows one transcript file and emits new events as they appear.
type Tailer struct {
	path     string
	interval time.Duration

	mu       sync.Mutex
	offset   int64  // byte offset consumed so far
	seq      uint64 // last emitted seq
	partial  []byte // trailing bytes of an incomplete final line
	lastSize int64
	inode    uint64
}

// NewTailer follows path, starting from the beginning.
func NewTailer(path string, interval time.Duration) *Tailer {
	if interval <= 0 {
		interval = 750 * time.Millisecond
	}
	return &Tailer{path: path, interval: interval}
}

// SeekEnd skips existing content, for when the store already has it.
func (t *Tailer) SeekEnd(fromSeq uint64) error {
	fi, err := os.Stat(t.path)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.offset = fi.Size()
	t.lastSize = fi.Size()
	t.seq = fromSeq
	return nil
}

// Poll reads whatever has been appended since the last call.
//
// A torn final line -- normal, since Claude Code is writing as we read -- is
// held in a buffer and completed on the next poll rather than being dropped.
func (t *Tailer) Poll() ([]protocol.TranscriptEvent, Meta, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	fi, err := os.Stat(t.path)
	if err != nil {
		return nil, Meta{}, err
	}

	// Truncation or replacement (a resumed session can rewrite the file):
	// start over rather than reading from a stale offset into the middle of a
	// line.
	if fi.Size() < t.offset {
		t.offset, t.partial, t.seq = 0, nil, 0
	}
	if fi.Size() == t.offset {
		return nil, Meta{}, nil
	}

	f, err := os.Open(t.path)
	if err != nil {
		return nil, Meta{}, err
	}
	defer f.Close()

	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil, Meta{}, err
	}
	chunk, err := io.ReadAll(f)
	if err != nil {
		return nil, Meta{}, err
	}
	t.offset += int64(len(chunk))
	t.lastSize = fi.Size()

	data := append(t.partial, chunk...)
	// Keep any trailing incomplete line for next time.
	if idx := strings.LastIndexByte(string(data), '\n'); idx >= 0 {
		t.partial = append([]byte(nil), data[idx+1:]...)
		data = data[:idx+1]
	} else {
		t.partial = data
		return nil, Meta{}, nil
	}

	events, meta, err := ParseTranscript(strings.NewReader(string(data)))
	if err != nil {
		return nil, meta, err
	}
	// ParseTranscript numbers from 1 per call; continue the global sequence.
	for i := range events {
		t.seq++
		events[i].Seq = t.seq
	}
	return events, meta, nil
}

// Interval is the recommended poll cadence.
func (t *Tailer) Interval() time.Duration { return t.interval }

// Path is the file being followed.
func (t *Tailer) Path() string { return t.path }

// ProjectSlug converts a working directory into Claude Code's project
// directory name: the absolute path with every "/" replaced by "-".
// Verified against real directories, e.g.
//
//	/Users/sam/dev/muxscale -> -Users-sam-dev-muxscale
func ProjectSlug(cwd string) string {
	return strings.ReplaceAll(filepath.Clean(cwd), "/", "-")
}

// ProjectDir returns the transcript directory for a working directory.
func ProjectDir(cwd string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects", ProjectSlug(cwd)), nil
}

// FindTranscripts returns session files for a working directory, newest first.
// Used when a session was discovered by process scan and no hook has told us
// its transcript_path yet.
func FindTranscripts(cwd string) ([]string, error) {
	dir, err := ProjectDir(cwd)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	type fileInfo struct {
		path string
		mod  time.Time
	}
	var files []fileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })

	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.path
	}
	return out, nil
}

// TranscriptCandidate is one of a project's session files, carrying the facts
// needed to tell whether it belongs to a particular running process.
type TranscriptCandidate struct {
	Path      string
	SessionID string
	Started   time.Time // the first event's timestamp; zero if unreadable
	Modified  time.Time
}

// transcriptStartTolerance bounds how far a session's first event may sit from
// the process's start time and still count as an exact match.
//
// A fresh session writes its first line as it boots, so the gap is the second
// or two it takes the process to get there. The window is generous because
// `ps` reports start time to the second and a loaded machine is slow to boot.
const transcriptStartTolerance = 60 * time.Second

// TranscriptCandidates lists a project's session files, newest first.
//
// FindTranscripts returns paths alone, which leaves the caller picking by
// modification time -- and modification time cannot tell two agents in one
// project apart: whichever wrote last wins, so a newly discovered session
// adopts its neighbour's transcript and the phone shows the wrong
// conversation. Reading the first line of each file costs one small read per
// candidate and yields the session id and the time the session began, which
// together are what the join actually needs.
func TranscriptCandidates(cwd string) ([]TranscriptCandidate, error) {
	paths, err := FindTranscripts(cwd)
	if err != nil {
		return nil, err
	}
	out := make([]TranscriptCandidate, 0, len(paths))
	for _, p := range paths {
		c := TranscriptCandidate{Path: p}
		if fi, err := os.Stat(p); err == nil {
			c.Modified = fi.ModTime()
		}
		c.SessionID, c.Started = firstEntry(p)
		out = append(out, c)
	}
	return out, nil
}

// firstEntry reads a transcript's opening line for its session id and time.
//
// Only the first JSON object is decoded, so this is one small read per
// candidate rather than a parse of a file that can be hundreds of megabytes.
func firstEntry(path string) (sessionID string, started time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return "", time.Time{}
	}
	defer f.Close()
	var e entry
	if err := json.NewDecoder(f).Decode(&e); err != nil {
		return "", time.Time{}
	}
	return e.SessionID, parseTime(e.Timestamp)
}

// PickTranscript chooses the session file belonging to a process that started
// at started, excluding files already claimed by another live session.
//
// Three cases, in order, because they are not equally trustworthy:
//
//   - A fresh session writes its first event within moments of the process
//     starting. That is an exact join on two independently-known times, and it
//     wins outright.
//   - One unclaimed file and one discovered process: there is nothing to
//     choose between, and refusing to answer would leave a session muxscale
//     can see without its transcript.
//   - Otherwise the session was resumed (claude --resume reuses a file that
//     already existed), so its first event predates the process. The file
//     written most recently *after* this process began is the closest thing to
//     an answer, and it is the only discriminator available.
//
// Returning "" is a valid outcome: the caller then leaves the session without a
// transcript rather than attaching it to someone else's conversation.
func PickTranscript(cands []TranscriptCandidate, started time.Time, claimed map[string]bool) string {
	free := make([]TranscriptCandidate, 0, len(cands))
	for _, c := range cands {
		if !claimed[c.Path] {
			free = append(free, c)
		}
	}
	if len(free) == 0 {
		return ""
	}

	if !started.IsZero() {
		best, bestDelta := "", time.Duration(0)
		for _, c := range free {
			if c.Started.IsZero() {
				continue
			}
			d := c.Started.Sub(started).Abs()
			if d > transcriptStartTolerance {
				continue
			}
			if best == "" || d < bestDelta {
				best, bestDelta = c.Path, d
			}
		}
		if best != "" {
			return best
		}
	}

	if len(free) == 1 {
		return free[0].Path
	}

	if started.IsZero() {
		return free[0].Path // FindTranscripts orders newest first
	}
	best, bestDelta := "", time.Duration(0)
	for _, c := range free {
		if c.Modified.IsZero() || c.Modified.Before(started) {
			continue
		}
		d := c.Modified.Sub(started)
		if best == "" || d < bestDelta {
			best, bestDelta = c.Path, d
		}
	}
	return best
}

// longToolCall is how long a tool call with no result still reads as running.
const longToolCall = 10 * time.Minute

// InferState derives a state from a transcript alone, for sessions with no
// hooks (started before muxscale, or on a version below the floor).
//
// Confidence is capped well below the hook path on purpose: this is inference,
// and the product's credibility depends on not dressing a guess up as a fact.
func InferState(events []protocol.TranscriptEvent, lastMod time.Time, quiescence time.Duration) protocol.StateGuess {
	now := time.Now()
	idle := now.Sub(lastMod)

	if len(events) == 0 {
		return protocol.StateGuess{
			State: protocol.StateUnknown, Confidence: 0.3,
			Reason: "no transcript events yet", Evidence: "transcript:empty", At: now,
		}
	}

	last := events[len(events)-1]

	// A tool call with no result yet means work is in flight.
	if last.Kind == protocol.EventToolCall && idle < quiescence {
		return protocol.StateGuess{
			State: protocol.StateRunning, Confidence: 0.7,
			Reason:   "a tool call is in flight",
			Evidence: "transcript:trailing tool_use " + last.ToolName, At: now,
		}
	}

	// Still no result, but quiet: a long tool -- a build, a test run -- is
	// the usual reason, and calling it unknown made a session flick between
	// Working and Idle for the whole of every slow command. Past longToolCall
	// it is more likely stuck, or sitting at a permission prompt, than working.
	if last.Kind == protocol.EventToolCall && idle < longToolCall {
		return protocol.StateGuess{
			State: protocol.StateRunning, Confidence: 0.55,
			Reason:   "a tool call has been running for " + idle.Round(time.Second).String(),
			Evidence: "transcript:trailing tool_use " + last.ToolName, At: now,
		}
	}

	if idle < quiescence {
		return protocol.StateGuess{
			State: protocol.StateRunning, Confidence: 0.65,
			Reason:   "transcript changed recently",
			Evidence: "transcript:idle " + idle.Round(time.Second).String(), At: now,
		}
	}

	// Quiet, and the last thing said was the assistant's: most likely waiting.
	if last.Kind == protocol.EventAssistantMessage {
		return protocol.StateGuess{
			State: protocol.StateWaitingInput, Confidence: 0.6,
			Reason:   "quiet after an assistant message",
			Evidence: "transcript:idle " + idle.Round(time.Second).String(), At: now,
		}
	}

	return protocol.StateGuess{
		State: protocol.StateUnknown, Confidence: 0.4,
		Reason:   "quiet, but the last entry is not a turn boundary",
		Evidence: "transcript:idle " + idle.Round(time.Second).String() + " last=" + string(last.Kind),
		At:       now,
	}
}
