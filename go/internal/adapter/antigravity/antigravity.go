// Package antigravity adapts Google Antigravity CLI (`agy`).
//
// Verified against Antigravity CLI 1.5.13 on a real installation:
//
//   - `~/.gemini/antigravity-cli/conversation_summaries.db` (SQLite, WAL)
//     indexes conversations: conversation_id, title, workspace_uris (JSON array
//     of file:// URIs), status, parent_conversation_id, nesting_depth,
//     last_modified_time. Subagents have parent_conversation_id / nesting_depth
//     set and are skipped during top-level discovery.
//   - `~/.gemini/antigravity-cli/brain/<id>/.system_generated/logs/transcript_full.jsonl`
//     holds the append-only conversation steps (USER_INPUT, PLANNER_RESPONSE,
//     GENERIC tool results, SYSTEM_MESSAGE). Unlike `transcript.jsonl`, it never
//     truncates long fields, so muxscale's own excerpting and "See all" work.
//   - `~/.gemini/antigravity-cli/presence/<id>.lock` is flock-held by the
//     running `agy` process that owns the conversation, giving exact liveness
//     without guessing from file mtime.
//   - `~/.gemini/config/hooks.json` supports global command hooks
//     (SessionStart, PreInvocation, PostInvocation, PreToolUse, PostToolUse,
//     Stop). muxscale wires SessionStart, PreInvocation and Stop for exact turn
//     boundaries and immediate PID/transcript joining. PreToolUse is
//     deliberately not wired: Antigravity treats an empty stdout `{}` from a
//     PreToolUse hook as a tool denial, so wiring it would break fail-open.
package antigravity

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

const (
	// MinVersion is the floor verified against a real installation.
	MinVersion = "1.5.0"
	// NewestTested is the highest version this build has fixtures for.
	NewestTested = "1.5.13"

	discoverLimit = 200
	liveWindow    = 12 * time.Hour

	// excerptBytes matches the other adapters so the daemon stores messages
	// whole up to 256 KB (for See all) and clips non-message kinds itself.
	excerptBytes = 256 * 1024

	// unansweredAfter is how long a user prompt or tool result may sit with no
	// follow-up step before inference stops calling the session busy.
	unansweredAfter = 60 * time.Second
)

// DataDir returns Antigravity CLI's data directory: $ANTIGRAVITY_CLI_HOME when
// set, else ~/.gemini/antigravity-cli.
func DataDir() (string, error) {
	if d := os.Getenv("ANTIGRAVITY_CLI_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "antigravity-cli"), nil
}

// ConfigDir returns Antigravity's global customization directory:
// $GEMINI_CONFIG_DIR when set, else ~/.gemini/config.
func ConfigDir() (string, error) {
	if d := os.Getenv("GEMINI_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "config"), nil
}

// SummariesDBPath is ~/.gemini/antigravity-cli/conversation_summaries.db.
func SummariesDBPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "conversation_summaries.db"), nil
}

// TranscriptPath returns the canonical transcript_full.jsonl path for a
// conversation ID, falling back to transcript.jsonl when only that exists.
func TranscriptPath(dataDir, convID string) string {
	logs := filepath.Join(dataDir, "brain", convID, ".system_generated", "logs")
	full := filepath.Join(logs, "transcript_full.jsonl")
	if _, err := os.Stat(full); err == nil {
		return full
	}
	compact := filepath.Join(logs, "transcript.jsonl")
	if _, err := os.Stat(compact); err == nil {
		return compact
	}
	return full
}

// ConversationIDFromPath extracts the conversation ID from a transcript path
// under .../brain/<id>/.system_generated/logs/transcript[_full].jsonl.
func ConversationIDFromPath(path string) string {
	if path == "" {
		return ""
	}
	clean := filepath.Clean(path)
	logsDir := filepath.Dir(clean)
	if filepath.Base(logsDir) != "logs" {
		return ""
	}
	sysDir := filepath.Dir(logsDir)
	if filepath.Base(sysDir) != ".system_generated" {
		return ""
	}
	convDir := filepath.Dir(sysDir)
	if filepath.Base(filepath.Dir(convDir)) != "brain" {
		return ""
	}
	id := filepath.Base(convDir)
	if id == "" || id == "." || id == "/" {
		return ""
	}
	return id
}

// dataDirFromTranscript derives the antigravity-cli root from a transcript
// path so liveness and summary lookups work for custom data directories too.
func dataDirFromTranscript(path string) string {
	if ConversationIDFromPath(path) == "" {
		return ""
	}
	// .../antigravity-cli/brain/<id>/.system_generated/logs/transcript_full.jsonl
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Clean(path))))))
}

// DetectVersion runs `agy --version`, which prints e.g. "1.5.13".
func DetectVersion() (adapter.Version, error) {
	bin := "agy"
	if _, err := exec.LookPath(bin); err != nil {
		if _, err2 := exec.LookPath("antigravity-cli"); err2 == nil {
			bin = "antigravity-cli"
		} else {
			return adapter.Version{}, err
		}
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return adapter.Version{}, err
	}
	return adapter.ParseVersion(string(out))
}

// ---- liveness via presence/<id>.lock ----

// Live reports whether an Antigravity process currently holds the presence
// flock for the conversation referenced by ref.
//
// Existence alone does not mean live: an exited session leaves its empty lock
// file behind. A non-blocking LOCK_EX fails with EWOULDBLOCK exactly when a
// live process holds the lease. A missing lock file means "not live"; any
// unexpected error returns unknown (false, false) so a permissions issue never
// marks every session ended.
func Live(ref string) (bool, bool) {
	id := ConversationIDFromPath(ref)
	if id == "" {
		return false, false
	}
	root := dataDirFromTranscript(ref)
	if root == "" {
		var err error
		root, err = DataDir()
		if err != nil {
			return false, false
		}
	}
	return liveByID(root, id)
}

func liveByID(dataDir, convID string) (bool, bool) {
	lock := filepath.Join(dataDir, "presence", convID+".lock")
	f, err := os.Open(lock)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, true
		}
		return false, false
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, true
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, true
	}
	return false, false
}

// ---- conversation_summaries.db ----

type summaryRow struct {
	ID           string
	Title        string
	Cwd          string
	Status       string
	LastModified time.Time
	LastInput    time.Time
}

func openSummariesDB(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(3000)&_pragma=query_only(1)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening %s read-only: %w", path, err)
	}
	return db, nil
}

func readSummaries(dataDir string, limit int) ([]summaryRow, error) {
	dbPath := filepath.Join(dataDir, "conversation_summaries.db")
	db, err := openSummariesDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
SELECT conversation_id,
       coalesce(title, ''),
       coalesce(workspace_uris, ''),
       coalesce(status, ''),
       coalesce(CAST(last_modified_time AS TEXT), ''),
       coalesce(CAST(last_user_input_time AS TEXT), '')
FROM conversation_summaries
WHERE coalesce(parent_conversation_id, '') = '' AND coalesce(nesting_depth, 0) = 0
ORDER BY last_modified_time DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []summaryRow
	for rows.Next() {
		var r summaryRow
		var urisJSON, modStr, inputStr string
		if err := rows.Scan(&r.ID, &r.Title, &urisJSON, &r.Status, &modStr, &inputStr); err != nil {
			return nil, err
		}
		if r.ID == "" {
			continue
		}
		r.Cwd = firstWorkspacePath(urisJSON)
		r.LastModified = parseTime(modStr)
		r.LastInput = parseTime(inputStr)
		out = append(out, r)
	}
	return out, rows.Err()
}

func readSummaryTitle(dataDir, convID string) string {
	if dataDir == "" || convID == "" {
		return ""
	}
	db, err := openSummariesDB(filepath.Join(dataDir, "conversation_summaries.db"))
	if err != nil {
		return ""
	}
	defer db.Close()
	var title string
	if err := db.QueryRow(`SELECT coalesce(title, '') FROM conversation_summaries WHERE conversation_id = ?`, convID).Scan(&title); err != nil {
		return ""
	}
	return strings.TrimSpace(title)
}

func firstWorkspacePath(urisJSON string) string {
	var uris []string
	if err := json.Unmarshal([]byte(urisJSON), &uris); err != nil {
		return ""
	}
	for _, raw := range uris {
		if p := fileURIToPath(raw); p != "" {
			return p
		}
	}
	return ""
}

func fileURIToPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.HasPrefix(raw, "file://") {
		return filepath.Clean(raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return strings.TrimPrefix(raw, "file://")
	}
	if u.Path != "" {
		return filepath.Clean(u.Path)
	}
	return ""
}

var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05-07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05",
}

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// discoverFromBrain is the fallback when conversation_summaries.db is absent.
func discoverFromBrain(dataDir string) ([]adapter.Discovered, error) {
	brainDir := filepath.Join(dataDir, "brain")
	entries, err := os.ReadDir(brainDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []adapter.Discovered
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		ref := TranscriptPath(dataDir, id)
		fi, err := os.Stat(ref)
		if err != nil {
			continue
		}
		live, known := liveByID(dataDir, id)
		if !known {
			live = time.Since(fi.ModTime()) < liveWindow
		}
		title := firstPromptFromFile(ref)
		out = append(out, adapter.Discovered{
			NaturalKey: id,
			Title:      title,
			Ref:        ref,
			Live:       live,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

func firstPromptFromFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for i := 0; i < 8 && sc.Scan(); i++ {
		var s stepRecord
		if json.Unmarshal(sc.Bytes(), &s) != nil {
			continue
		}
		if s.Type == "USER_INPUT" {
			if txt := extractUserRequest(s.Content); txt != "" {
				return oneLine(txt, 80)
			}
		}
	}
	return ""
}

// ---- transcript parsing & tailing ----

type stepRecord struct {
	StepIndex       int        `json:"step_index"`
	Source          string     `json:"source"`
	Type            string     `json:"type"`
	Status          string     `json:"status"`
	CreatedAt       string     `json:"created_at"`
	Content         string     `json:"content"`
	Thinking        string     `json:"thinking"`
	Model           string     `json:"model"`
	Error           string     `json:"error"`
	InputTokens     int        `json:"input_tokens"`
	CacheReadTokens int        `json:"cache_read_tokens"`
	OutputTokens    int        `json:"output_tokens"`
	ToolCalls       []toolCall `json:"tool_calls"`
}

type toolCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type pendingTool struct {
	id   string
	name string
}

// Lifecycle names reported via adapter.EventReporter for InferState.
const (
	lifeUserInput       = "user_input"
	lifeToolCalls       = "planner:tool_calls"
	lifeToolDone        = "tool_result"
	lifeDone            = "planner:done"
	lifeError           = "planner:error"
	lifeWaitingApproval = "waiting:approval"
	lifeWaitingQuestion = "waiting:question"

	// stepStatusWaiting is Antigravity's CORTEX_STEP_STATUS_WAITING enum value
	// in conversations/<id>.db `steps.status`, set while a permission prompt or
	// ask_question dialog is waiting on a human.
	stepStatusWaiting = 9
)

// stream follows one Antigravity transcript_full.jsonl incrementally.
type stream struct {
	path    string
	dataDir string
	convID  string

	mu           sync.Mutex
	offset       int64
	seq          uint64
	partial      []byte
	pendingTools []pendingTool
	life         string

	lastModel string
	lastTitle string
	sawTitle  bool
	meta      adapter.Meta

	convChecked bool
	convDBMod   time.Time
	convDBSize  int64
	convWALMod  time.Time
	convWALSize int64
	pending     *adapter.PendingPrompt
}

func newStream(path string) *stream {
	return &stream{
		path:    path,
		dataDir: dataDirFromTranscript(path),
		convID:  ConversationIDFromPath(path),
	}
}

var (
	_ adapter.TranscriptStream    = (*stream)(nil)
	_ adapter.SeekableStream      = (*stream)(nil)
	_ adapter.MetaSource          = (*stream)(nil)
	_ adapter.EventReporter       = (*stream)(nil)
	_ adapter.PendingPromptSource = (*stream)(nil)
)

func (s *stream) Close() error { return nil }

func (s *stream) LastEvent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != nil {
		if s.pending.ToolName == "ask_question" {
			return lifeWaitingQuestion
		}
		return lifeWaitingApproval
	}
	return s.life
}

func (s *stream) PendingPrompt() *adapter.PendingPrompt {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return nil
	}
	cp := *s.pending
	return &cp
}

func (s *stream) Meta() adapter.Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.meta
	s.meta = adapter.Meta{}
	return m
}

// SeekEnd skips existing lines that the store already holds while rebuilding
// tool-call pairing and lifecycle state so later lines continue cleanly.
func (s *stream) SeekEnd(fromSeq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fi, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.seq = fromSeq
			return nil
		}
		return err
	}
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	s.offset = int64(len(data))
	s.partial = nil
	s.pendingTools = nil
	s.life = ""
	s.seq = fromSeq

	lines, rem := splitLines(data)
	s.partial = rem
	for _, line := range lines {
		_, _ = s.parseLine(line, false)
	}
	// If the file grew between Stat and ReadAll, offset matches what we read.
	_ = fi
	return nil
}

func (s *stream) Poll(context.Context) ([]protocol.TranscriptEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fi, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Handle truncation or file replacement cleanly.
	if fi.Size() < s.offset {
		s.offset, s.partial, s.seq = 0, nil, 0
		s.pendingTools = nil
		s.life = ""
	}

	if fi.Size() == s.offset {
		s.refreshDBTitle()
		s.refreshPendingPrompt()
		return nil, nil
	}

	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(s.offset, io.SeekStart); err != nil {
		return nil, err
	}
	chunk, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	s.offset += int64(len(chunk))

	buf := append(s.partial, chunk...)
	lines, rem := splitLines(buf)
	s.partial = rem

	var events []protocol.TranscriptEvent
	for _, line := range lines {
		evs, _ := s.parseLine(line, true)
		events = append(events, evs...)
	}
	s.refreshDBTitle()
	s.refreshPendingPrompt()
	return events, nil
}

// refreshPendingPrompt inspects conversations/<convID>.db for any step in
// status=9 (WAITING), skipping the SQLite read when neither the .db nor its
// -wal file changed since the previous poll.
func (s *stream) refreshPendingPrompt() {
	if s.dataDir == "" || s.convID == "" {
		return
	}
	dbPath := filepath.Join(s.dataDir, "conversations", s.convID+".db")
	dbFI, err := os.Stat(dbPath)
	if err != nil {
		s.pending = nil
		return
	}
	var walMod time.Time
	var walSize int64
	if walFI, err := os.Stat(dbPath + "-wal"); err == nil {
		walMod, walSize = walFI.ModTime(), walFI.Size()
	}
	if s.convChecked &&
		dbFI.ModTime().Equal(s.convDBMod) && dbFI.Size() == s.convDBSize &&
		walMod.Equal(s.convWALMod) && walSize == s.convWALSize {
		return
	}
	p, ok := readWaitingStep(dbPath)
	if !ok {
		return
	}
	s.convChecked = true
	s.convDBMod, s.convDBSize = dbFI.ModTime(), dbFI.Size()
	s.convWALMod, s.convWALSize = walMod, walSize
	s.pending = p
}

// readWaitingStep queries conversations/<id>.db read-only for the latest step
// blocked on user approval or input (status = 9). Returns (prompt, true) when
// the database was read cleanly (prompt is nil if no step is waiting), or
// (nil, false) on a transient read error.
func readWaitingStep(dbPath string) (*adapter.PendingPrompt, bool) {
	db, err := openSummariesDB(dbPath)
	if err != nil {
		return nil, false
	}
	defer db.Close()

	var idx int
	var meta []byte
	err = db.QueryRow(`SELECT idx, metadata FROM steps WHERE status = ? ORDER BY idx DESC LIMIT 1`, stepStatusWaiting).Scan(&idx, &meta)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	p := parseStepMetadata(idx, meta)
	if p == nil || p.ToolName == "" {
		return nil, true
	}
	return p, true
}

// parseStepMetadata decodes the minimal protobuf wire fields muxscale needs
// from a waiting step's metadata blob:
//   - field 1: Timestamp { field 1: seconds (varint), field 2: nanos (varint) }
//   - field 4: ToolCall  { field 1: id, field 2: name, field 3: args_json }
//   - field 34: PermissionInfo { field 5: reason }
func parseStepMetadata(idx int, data []byte) *adapter.PendingPrompt {
	var p adapter.PendingPrompt
	walkProto(data, func(field int, wtype byte, varint uint64, raw []byte) {
		if wtype != 2 {
			return
		}
		switch field {
		case 1:
			var sec, nanos int64
			walkProto(raw, func(sf int, swt byte, sv uint64, _ []byte) {
				if swt != 0 {
					return
				}
				if sf == 1 {
					sec = int64(sv)
				} else if sf == 2 {
					nanos = int64(sv)
				}
			})
			if sec > 0 {
				p.AskedAt = time.Unix(sec, nanos)
			}
		case 4:
			walkProto(raw, func(sf int, swt byte, _ uint64, sraw []byte) {
				if swt != 2 {
					return
				}
				switch sf {
				case 1:
					p.ToolUseID = string(sraw)
				case 2:
					p.ToolName = string(sraw)
				case 3:
					var args map[string]any
					if json.Unmarshal(sraw, &args) == nil {
						p.ToolInput = args
					}
				}
			})
		case 34:
			walkProto(raw, func(sf int, swt byte, _ uint64, sraw []byte) {
				if swt == 2 && sf == 5 {
					p.Reason = strings.TrimSpace(string(sraw))
				}
			})
		}
	})
	if p.ToolName == "" {
		return nil
	}
	if p.ToolUseID == "" {
		p.ToolUseID = "step-" + strconv.Itoa(idx)
	}
	return &p
}

func walkProto(data []byte, fn func(field int, wtype byte, varint uint64, raw []byte)) {
	for i := 0; i < len(data); {
		tag, n := readVarint(data[i:])
		if n == 0 {
			return
		}
		i += n
		field := int(tag >> 3)
		wtype := byte(tag & 7)
		switch wtype {
		case 0:
			v, m := readVarint(data[i:])
			if m == 0 {
				return
			}
			i += m
			fn(field, wtype, v, nil)
		case 1:
			if i+8 > len(data) {
				return
			}
			fn(field, wtype, 0, data[i:i+8])
			i += 8
		case 2:
			ln, m := readVarint(data[i:])
			if m == 0 || i+m+int(ln) > len(data) || int(ln) < 0 {
				return
			}
			i += m
			fn(field, wtype, 0, data[i:i+int(ln)])
			i += int(ln)
		case 5:
			if i+4 > len(data) {
				return
			}
			fn(field, wtype, 0, data[i:i+4])
			i += 4
		default:
			return
		}
	}
}

func readVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		c := b[i]
		v |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

// refreshDBTitle checks conversation_summaries.db for Antigravity's generated
// title when one has not been reported yet (or changed).
func (s *stream) refreshDBTitle() {
	if s.dataDir == "" || s.convID == "" {
		return
	}
	// Once Antigravity has written its real AI title, avoid polling SQLite on
	// every idle tick.
	if s.sawTitle {
		return
	}
	if title := readSummaryTitle(s.dataDir, s.convID); title != "" {
		s.sawTitle = true
		if title != s.lastTitle {
			s.lastTitle = title
			s.meta.Title = title
		}
	}
}

func splitLines(buf []byte) (lines [][]byte, remainder []byte) {
	start := 0
	for i := 0; i < len(buf); i++ {
		if buf[i] == '\n' {
			line := buf[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if len(line) > 0 {
				lines = append(lines, line)
			}
			start = i + 1
		}
	}
	if start < len(buf) {
		remainder = append([]byte(nil), buf[start:]...)
	}
	return lines, remainder
}

func (s *stream) nextSeq() uint64 {
	s.seq++
	return s.seq
}

// parseLine parses one JSONL step. When emitEvents is false (used by SeekEnd),
// it updates pairing, metadata and lifecycle without allocating events.
func (s *stream) parseLine(line []byte, emitEvents bool) ([]protocol.TranscriptEvent, bool) {
	var rec stepRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil, false
	}
	at := parseTime(rec.CreatedAt)
	if at.IsZero() {
		at = time.Now()
	}
	baseID := "step-" + strconv.Itoa(rec.StepIndex)

	var out []protocol.TranscriptEvent
	add := func(ev protocol.TranscriptEvent) {
		if !emitEvents {
			return
		}
		ev.Seq = s.nextSeq()
		out = append(out, ev)
	}

	switch rec.Type {
	case "USER_INPUT":
		text := extractUserRequest(rec.Content)
		if strings.TrimSpace(text) == "" {
			return nil, false
		}
		// A new user prompt starts a fresh turn; discard any orphaned tool
		// calls left behind by an interrupted turn.
		s.pendingTools = nil
		s.life = lifeUserInput
		if s.lastTitle == "" {
			s.lastTitle = oneLine(text, 80)
			if emitEvents {
				s.meta.Title = s.lastTitle
			}
		}
		clipped, trunc := clip(text, excerptBytes)
		add(protocol.TranscriptEvent{
			ID:        baseID,
			Kind:      protocol.EventUserMessage,
			At:        at,
			Text:      clipped,
			Truncated: trunc,
		})
		return out, true

	case "PLANNER_RESPONSE":
		if rec.Model != "" && rec.Model != s.lastModel {
			s.lastModel = rec.Model
			if emitEvents {
				s.meta.Model = rec.Model
			}
		}
		var usage *protocol.Usage
		if rec.InputTokens > 0 || rec.OutputTokens > 0 || rec.CacheReadTokens > 0 {
			usage = &protocol.Usage{
				InputTokens:     rec.InputTokens,
				OutputTokens:    rec.OutputTokens,
				CacheReadTokens: rec.CacheReadTokens,
			}
		}
		if t := strings.TrimSpace(rec.Thinking); t != "" {
			clipped, trunc := clip(t, excerptBytes)
			add(protocol.TranscriptEvent{
				ID:        baseID + ":thinking",
				Kind:      protocol.EventThinking,
				At:        at,
				Text:      clipped,
				Model:     rec.Model,
				Truncated: trunc,
			})
		}
		if c := strings.TrimSpace(rec.Content); c != "" {
			clipped, trunc := clip(c, excerptBytes)
			add(protocol.TranscriptEvent{
				ID:        baseID + ":text",
				Kind:      protocol.EventAssistantMessage,
				At:        at,
				Text:      clipped,
				Model:     rec.Model,
				Usage:     usage,
				Truncated: trunc,
			})
		}
		for i, tc := range rec.ToolCalls {
			callID := tc.ID
			if callID == "" {
				callID = fmt.Sprintf("%s-call-%d", baseID, i)
			}
			s.pendingTools = append(s.pendingTools, pendingTool{id: callID, name: tc.Name})
			add(protocol.TranscriptEvent{
				ID:        callID,
				Kind:      protocol.EventToolCall,
				At:        at,
				ToolName:  tc.Name,
				ToolUseID: callID,
				ToolInput: tc.Args,
				Model:     rec.Model,
			})
		}
		if rec.Status == "ERROR" || rec.Error != "" {
			s.life = lifeError
			errText := strings.TrimSpace(rec.Error)
			if errText == "" {
				errText = "step failed"
			}
			clipped, trunc := clip(errText, excerptBytes)
			add(protocol.TranscriptEvent{
				ID:        baseID + ":error",
				Kind:      protocol.EventError,
				At:        at,
				Text:      clipped,
				IsError:   true,
				Truncated: trunc,
			})
		} else if len(rec.ToolCalls) > 0 {
			s.life = lifeToolCalls
		} else {
			s.life = lifeDone
		}
		return out, true

	case "GENERIC":
		if rec.Source != "MODEL" {
			return nil, false
		}
		var pt pendingTool
		if len(s.pendingTools) > 0 {
			pt = s.pendingTools[0]
			s.pendingTools = s.pendingTools[1:]
		} else {
			pt = pendingTool{id: baseID}
		}
		isErr := rec.Status == "ERROR" || rec.Error != ""
		body := cleanToolOutput(rec.Content)
		if body == "" && rec.Error != "" {
			body = rec.Error
		}
		clipped, trunc := clip(body, excerptBytes)
		add(protocol.TranscriptEvent{
			ID:         baseID + ":result",
			Kind:       protocol.EventToolResult,
			At:         at,
			ToolName:   pt.name,
			ToolUseID:  pt.id,
			ToolOutput: clipped,
			Text:       clipped,
			IsError:    isErr,
			Truncated:  trunc,
		})
		s.life = lifeToolDone
		return out, true

	case "SYSTEM_MESSAGE":
		// Internal harness reminders (e.g. <SYSTEM_MESSAGE>) are not part of
		// the human/assistant conversation.
		return nil, false

	case "COMPACTION", "CHECKPOINT":
		text := strings.TrimSpace(rec.Content)
		if text == "" {
			text = "Antigravity compacted the conversation"
		}
		clipped, trunc := clip(text, excerptBytes)
		add(protocol.TranscriptEvent{
			ID:        baseID,
			Kind:      protocol.EventCompaction,
			At:        at,
			Text:      clipped,
			Truncated: trunc,
		})
		return out, true

	default:
		if rec.Status == "ERROR" || rec.Error != "" {
			s.life = lifeError
			errText := strings.TrimSpace(firstNonEmpty(rec.Error, rec.Content))
			if errText != "" {
				clipped, trunc := clip(errText, excerptBytes)
				add(protocol.TranscriptEvent{
					ID:        baseID + ":error",
					Kind:      protocol.EventError,
					At:        at,
					Text:      clipped,
					IsError:   true,
					Truncated: trunc,
				})
				return out, true
			}
		}
		return nil, false
	}
}

// extractUserRequest pulls the human's actual message out of Antigravity's
// <USER_REQUEST>...</USER_REQUEST> envelope, dropping appended metadata tags
// (<ADDITIONAL_METADATA>, <USER_SETTINGS_CHANGE>, etc.). When the tag is
// absent, the content is returned trimmed as-is.
func extractUserRequest(content string) string {
	const openTag = "<USER_REQUEST>"
	const closeTag = "</USER_REQUEST>"
	if start := strings.Index(content, openTag); start >= 0 {
		rest := content[start+len(openTag):]
		if end := strings.Index(rest, closeTag); end >= 0 {
			return strings.TrimSpace(rest[:end])
		}
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(content)
}

// cleanToolOutput strips leading `Created At:` / `Completed At:` boilerplate
// lines that Antigravity prepends to tool results.
func cleanToolOutput(s string) string {
	for {
		line, rest, hasNL := strings.Cut(s, "\n")
		trimmed := strings.TrimRight(line, "\r")
		if strings.HasPrefix(trimmed, "Created At: ") || strings.HasPrefix(trimmed, "Completed At: ") {
			if !hasNL {
				return ""
			}
			s = rest
			continue
		}
		break
	}
	return strings.TrimSpace(s)
}

// ---- state inference ----

func stateFromLifecycle(in adapter.StateInput) (protocol.StateGuess, bool) {
	now := time.Now()
	g := func(st protocol.SessionState, conf float64, reason string) (protocol.StateGuess, bool) {
		return protocol.StateGuess{
			State: st, Confidence: conf, Reason: reason,
			Evidence: "antigravity " + in.Event, At: now,
		}, true
	}

	if in.Live != nil && !*in.Live {
		return protocol.StateGuess{
			State: protocol.StateExited, Confidence: 0.95,
			Reason:   "Antigravity no longer holds this conversation",
			Evidence: "antigravity presence:unlocked", At: now,
		}, true
	}

	switch in.Event {
	case lifeWaitingApproval:
		return g(protocol.StateWaitingApproval, 0.9, "waiting for tool approval")
	case lifeWaitingQuestion:
		return g(protocol.StateWaitingInput, 0.9, "waiting for an answer to a question")
	case lifeDone:
		return g(protocol.StateWaitingInput, 0.85, "the turn finished")
	case lifeError:
		return g(protocol.StateWaitingInput, 0.8, "the turn failed")
	case lifeToolCalls:
		if in.Idle < in.Quiescence {
			return g(protocol.StateRunning, 0.65, "running a tool")
		}
		return g(protocol.StateRunning, 0.55, "a tool is running, or waiting for approval")
	case lifeToolDone, lifeUserInput:
		if in.Idle < in.Quiescence {
			return g(protocol.StateRunning, 0.65, "the agent is working")
		}
		if in.Idle >= unansweredAfter {
			return g(protocol.StateWaitingInput, 0.55, "quiet after last step; may have been interrupted")
		}
		return g(protocol.StateRunning, 0.55, "waiting for the next model response")
	}

	if len(in.Recent) > 0 {
		last := in.Recent[len(in.Recent)-1]
		switch last.Kind {
		case protocol.EventAssistantMessage:
			return protocol.StateGuess{
				State: protocol.StateWaitingInput, Confidence: 0.8,
				Reason: "assistant replied", Evidence: "antigravity transcript", At: now,
			}, true
		case protocol.EventToolCall:
			return protocol.StateGuess{
				State: protocol.StateRunning, Confidence: 0.6,
				Reason: "tool call in progress", Evidence: "antigravity transcript", At: now,
			}, true
		case protocol.EventUserMessage, protocol.EventToolResult:
			if in.Idle >= unansweredAfter {
				return protocol.StateGuess{
					State: protocol.StateWaitingInput, Confidence: 0.55,
					Reason: "quiet after last step", Evidence: "antigravity transcript", At: now,
				}, true
			}
			return protocol.StateGuess{
				State: protocol.StateRunning, Confidence: 0.6,
				Reason: "agent is working", Evidence: "antigravity transcript", At: now,
			}, true
		}
	}
	return protocol.StateGuess{}, false
}

// ---- small helpers ----

func clip(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
