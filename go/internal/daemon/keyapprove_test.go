package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/adapter/claudecode"
	"github.com/muxscale/muxscale/go/internal/adapter/omp"
	"github.com/muxscale/muxscale/go/internal/protocol"
	"github.com/muxscale/muxscale/go/internal/store"
)

// ompApprovals is a daemon with omp's adapter, its hook endpoint and the
// control API on one mux, and a record of every key typed into a session.
type ompApprovals struct {
	t     *testing.T
	mux   *http.ServeMux
	reg   *Registry
	mu    sync.Mutex
	typed []string
	fail  error
}

func newOMPApprovals(t *testing.T) *ompApprovals {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := NewRegistry()
	adapters := adapter.NewRegistry()
	adapters.MustRegister(omp.New())
	m := NewManager(reg, adapters, st, log)
	o := &ompApprovals{t: t, mux: http.NewServeMux(), reg: reg}
	m.typeKeys = func(_ string, keys []byte) error {
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.fail != nil {
			return o.fail
		}
		o.typed = append(o.typed, string(keys))
		return nil
	}
	hs := NewHookServer(reg, adapters, "hook-token", time.Second, log)
	hs.SetManager(m)
	hs.Routes(o.mux)
	d := &Daemon{cfg: Config{LocalToken: testToken}, mgr: m, reg: reg, store: st, log: log}
	d.routes(o.mux)
	return o
}

func (o *ompApprovals) hook(event, toolUseID string, extra map[string]any) {
	o.t.Helper()
	body := map[string]any{"v": 1, "agent": "omp", "event": event, "session_id": "s1", "cwd": "/w", "tool_use_id": toolUseID}
	for k, v := range extra {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/hook/omp", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer hook-token")
	rec := httptest.NewRecorder()
	o.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		o.t.Fatalf("hook %s: %d", event, rec.Code)
	}
}

func (o *ompApprovals) decide(id, behavior string) *httptest.ResponseRecorder {
	o.t.Helper()
	b, _ := json.Marshal(map[string]string{"id": id, "behavior": behavior, "by": "phone"})
	req := httptest.NewRequest(http.MethodPost, "/v1/approvals/decide", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	o.mux.ServeHTTP(rec, req)
	return rec
}

func (o *ompApprovals) pending() []protocol.ApprovalRequest { return o.reg.PendingApprovals() }

func (o *ompApprovals) state() protocol.SessionState {
	id, _ := o.reg.IDFor("omp", "s1")
	s, _ := o.reg.Get(id)
	return s.State
}

func (o *ompApprovals) keys() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.Join(o.typed, "|")
}

// omp's prompt, reported by the extension, becomes a card; the phone's answer
// is typed as omp's own key; the prompt closing retires the card.
func TestOMPApprovalAnsweredFromThePhone(t *testing.T) {
	o := newOMPApprovals(t)
	o.hook("session_start", "", nil)
	o.hook("approval_requested", "call-1", map[string]any{
		"tool_name": "bash", "tool_input": map[string]any{"command": "rm -rf build"},
		"reason": "Critical pattern detected",
	})

	p := o.pending()
	if len(p) != 1 || p[0].Source != protocol.ApprovalHook || p[0].ToolUseID != "call-1" {
		t.Fatalf("pending = %+v", p)
	}
	if p[0].Subject != "bash: rm -rf build (Critical pattern detected)" || !p[0].Deadline.IsZero() {
		t.Errorf("card = %+v", p[0])
	}
	if o.state() != protocol.StateWaitingApproval {
		t.Fatalf("state = %s", o.state())
	}

	if rec := o.decide(p[0].ID, "allow"); rec.Code != http.StatusOK {
		t.Fatalf("decide: %d %s", rec.Code, rec.Body)
	}
	if o.keys() != "\x11" {
		t.Fatalf("typed %q, want Ctrl+Q", o.keys())
	}
	if len(o.pending()) != 0 {
		t.Fatal("the card stayed after the answer was typed")
	}
	// omp then reports its prompt closed; nothing more is typed.
	o.hook("approval_resolved", "call-1", map[string]any{"approved": true})
	if o.keys() != "\x11" || o.state() != protocol.StateRunning {
		t.Errorf("after close: keys %q state %s", o.keys(), o.state())
	}

	// A deny is Esc.
	o.hook("approval_requested", "call-2", map[string]any{"tool_name": "write", "tool_input": map[string]any{"path": "/etc/hosts"}})
	if rec := o.decide(o.pending()[0].ID, "deny"); rec.Code != http.StatusOK {
		t.Fatalf("deny: %d %s", rec.Code, rec.Body)
	}
	if o.keys() != "\x11|\x1b" {
		t.Errorf("typed %q", o.keys())
	}
}

// Answered at the keyboard first: the card goes, and nothing is typed.
func TestOMPApprovalAnsweredAtTheKeyboard(t *testing.T) {
	o := newOMPApprovals(t)
	o.hook("approval_requested", "call-1", map[string]any{"tool_name": "bash"})
	id := o.pending()[0].ID
	o.hook("approval_resolved", "call-1", map[string]any{"approved": false, "reason": "denied by user"})

	if len(o.pending()) != 0 || o.state() != protocol.StateRunning {
		t.Fatalf("pending %+v state %s", o.pending(), o.state())
	}
	if rec := o.decide(id, "allow"); rec.Code == http.StatusOK && o.keys() != "" {
		t.Fatalf("a late answer was typed: %q", o.keys())
	}
	if o.keys() != "" {
		t.Errorf("typed %q", o.keys())
	}
}

// The keys answer whatever prompt is on screen, which is the oldest. A later
// one cannot be answered first, and a failure to type leaves the card up.
func TestOMPApprovalOnlyTheOneOnScreen(t *testing.T) {
	o := newOMPApprovals(t)
	o.hook("approval_requested", "call-1", map[string]any{"tool_name": "bash"})
	o.hook("approval_requested", "call-2", map[string]any{"tool_name": "bash"})
	o.hook("approval_requested", "call-2", map[string]any{"tool_name": "bash"}) // repeated
	p := o.pending()
	if len(p) != 2 {
		t.Fatalf("pending = %d", len(p))
	}
	if rec := o.decide(p[1].ID, "allow"); rec.Code != http.StatusConflict || o.keys() != "" {
		t.Fatalf("second answered first: %d, typed %q", rec.Code, o.keys())
	}

	o.fail = errors.New("pane gone")
	if rec := o.decide(p[0].ID, "allow"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "pane gone") {
		t.Fatalf("failed typing: %d %s", rec.Code, rec.Body)
	}
	if len(o.pending()) != 2 {
		t.Fatal("a card was dropped though its answer never landed")
	}

	// Closing the first leaves the session waiting on the second.
	o.fail = nil
	o.hook("approval_resolved", "call-1", map[string]any{"approved": true})
	if o.state() != protocol.StateWaitingApproval {
		t.Errorf("state = %s with a prompt still open", o.state())
	}
	if rec := o.decide(p[1].ID, "deny"); rec.Code != http.StatusOK || o.keys() != "\x1b" {
		t.Errorf("now first: %d, typed %q", rec.Code, o.keys())
	}
}

// omp's queue arrives whole and replaces what was shown; an exit clears it.
func TestOMPQueueReachesTheSession(t *testing.T) {
	o := newOMPApprovals(t)
	o.hook("session_start", "", nil)
	o.hook("queue", "", map[string]any{"queued": []map[string]any{{"text": "also run the tests"}, {"text": "then push"}}})
	id, _ := o.reg.IDFor("omp", "s1")
	if s, _ := o.reg.Get(id); len(s.Queued) != 2 || s.Queued[1].Text != "then push" {
		t.Fatalf("queued = %+v", s.Queued)
	}
	o.hook("queue", "", map[string]any{"queued": []map[string]any{{"text": "then push"}}})
	if s, _ := o.reg.Get(id); len(s.Queued) != 1 {
		t.Fatalf("after one was taken = %+v", s.Queued)
	}
	o.hook("session_shutdown", "", nil)
	if s, _ := o.reg.Get(id); len(s.Queued) != 0 {
		t.Errorf("an exited session still shows %+v", s.Queued)
	}
}

// A Claude Code question that has fallen through to its terminal -- the hold
// ran out, or the daemon restarted -- stays on the phone while the transcript
// says it is unanswered, and the answer is typed into the dialog.
func TestKeyboardQuestionFollowsTheTranscript(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg := NewRegistry()
	adapters := adapter.NewRegistry()
	adapters.MustRegister(claudecode.New())
	m := NewManager(reg, adapters, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var typed []string
	m.typeKeys = func(_ string, keys []byte) error { typed = append(typed, string(keys)); return nil }

	sess := reg.Upsert("claude-code", "s1", nil)
	ask := protocol.TranscriptEvent{Kind: protocol.EventToolCall, ToolName: askUserQuestion, ToolUseID: "toolu_1",
		ToolInput: map[string]any{"questions": []any{map[string]any{
			"question": "Pick a color",
			"options":  []any{map[string]any{"label": "red"}, map[string]any{"label": "green"}},
		}}}}
	pending := func() []protocol.ApprovalRequest { return reg.SessionApprovals(sess.ID) }

	m.syncKeyboardQuestion(sess.ID, []protocol.TranscriptEvent{ask}, false)
	if len(pending()) != 0 {
		t.Fatal("posted while the transcript was still being read")
	}
	m.syncKeyboardQuestion(sess.ID, []protocol.TranscriptEvent{ask}, true)
	m.syncKeyboardQuestion(sess.ID, []protocol.TranscriptEvent{ask}, true)
	if p := pending(); len(p) != 1 || p[0].Source != protocol.ApprovalHook || p[0].Kind != "question" {
		t.Fatalf("want one keyboard question, got %+v", p)
	}

	id := pending()[0].ID
	err = m.AnswerByKeys(id, protocol.ApprovalDecision{Behavior: "allow",
		Answers: map[string]string{"Pick a color": "teal"}}, "phone")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(typed, "|") != "3|teal|\r" {
		t.Errorf("typed %q, want Type something, the words, then Enter", typed)
	}
	// The agent has not written the answer down yet.
	m.syncKeyboardQuestion(sess.ID, []protocol.TranscriptEvent{ask}, true)
	if len(pending()) != 0 {
		t.Error("asked again before the agent recorded the answer")
	}

	// Answered at the keyboard instead: the card goes.
	ask2 := ask
	ask2.ToolUseID = "toolu_2"
	m.syncKeyboardQuestion(sess.ID, []protocol.TranscriptEvent{ask2}, true)
	if len(pending()) != 1 {
		t.Fatal("a new question was not posted")
	}
	result := protocol.TranscriptEvent{Kind: protocol.EventToolResult, ToolUseID: "toolu_2"}
	m.syncKeyboardQuestion(sess.ID, []protocol.TranscriptEvent{ask2, result}, false)
	if len(pending()) != 0 {
		t.Error("the card outlived the question")
	}
}

// While a hook holds the question, the phone already has it as data.
func TestKeyboardQuestionDefersToAHeldOne(t *testing.T) {
	m, reg := reconcileFixture(t)
	adapters := adapter.NewRegistry()
	adapters.MustRegister(claudecode.New())
	m.adapters = adapters
	sess := reg.Upsert("claude-code", "s1", nil)
	reg.AskApproval(&protocol.ApprovalRequest{ID: "held", SessionID: sess.ID, Source: protocol.ApprovalNative, Kind: "question"})
	ask := protocol.TranscriptEvent{Kind: protocol.EventToolCall, ToolName: askUserQuestion, ToolUseID: "toolu_1",
		ToolInput: map[string]any{"questions": []any{map[string]any{"question": "Q", "options": []any{map[string]any{"label": "a"}}}}}}
	m.syncKeyboardQuestion(sess.ID, []protocol.TranscriptEvent{ask}, true)
	if p := reg.SessionApprovals(sess.ID); len(p) != 1 || p[0].ID != "held" {
		t.Errorf("want only the held question, got %+v", p)
	}
}
