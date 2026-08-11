package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

func event(t *testing.T, raw string) Event {
	t.Helper()
	var e Event
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	return e
}

// session.idle is OpenCode's stop signal, the counterpart of Claude Code's
// Stop, pi's agent_settled and Codex's task_complete.
func TestStateMapping(t *testing.T) {
	cases := map[string]protocol.SessionState{
		EventSessionIdle:       protocol.StateWaitingInput,
		EventPermissionAsked:   protocol.StateWaitingApproval,
		EventQuestionAsked:     protocol.StateWaitingApproval,
		EventPrompted:          protocol.StateRunning,
		EventToolCalled:        protocol.StateRunning,
		EventPermissionReplied: protocol.StateRunning,
	}
	for evt, want := range cases {
		g, ok := StateFromEvent(evt)
		if !ok {
			t.Errorf("%s produced no state", evt)
			continue
		}
		if g.State != want {
			t.Errorf("%s -> %s, want %s", evt, g.State, want)
		}
		if g.Confidence != 1.0 {
			t.Errorf("%s confidence = %v; OpenCode tells us directly", evt, g.Confidence)
		}
	}
	if _, ok := StateFromEvent(EventServerConnected); ok {
		t.Error("server.connected is not a session state change")
	}
}

func TestSessionIDExtraction(t *testing.T) {
	e := event(t, `{"id":"evt_1","type":"session.idle","properties":{"sessionID":"ses_abc"}}`)
	if got := e.SessionID(); got != "ses_abc" {
		t.Errorf("sessionID = %q", got)
	}
	bare := event(t, `{"id":"evt_2","type":"server.connected","data":{}}`)
	if got := bare.SessionID(); got != "" {
		t.Errorf("expected no session id, got %q", got)
	}
}

// Older builds put the payload under `data`, current ones under `properties`.
// Accepting both means a version bump does not blank out every event.
func TestEventAcceptsBothPayloadKeys(t *testing.T) {
	withProps := event(t, `{"type":"session.idle","properties":{"sessionID":"ses_1"}}`)
	withData := event(t, `{"type":"session.idle","data":{"sessionID":"ses_1"}}`)
	if withProps.SessionID() != "ses_1" || withData.SessionID() != "ses_1" {
		t.Error("both payload keys must be accepted")
	}
}

func TestToolEventsBecomeTranscript(t *testing.T) {
	called := event(t, `{"type":"session.next.tool.called","properties":{"sessionID":"ses_1","callID":"c1","tool":"bash","input":{"command":"ls"}}}`)
	evs := ToTranscript(called)
	if len(evs) != 1 || evs[0].Kind != protocol.EventToolCall {
		t.Fatalf("tool.called -> %+v", evs)
	}
	if evs[0].ToolName != "bash" || evs[0].ToolUseID != "c1" {
		t.Errorf("tool call = %+v", evs[0])
	}
	if evs[0].ToolInput["command"] != "ls" {
		t.Errorf("input lost: %v", evs[0].ToolInput)
	}

	failed := event(t, `{"type":"session.next.tool.failed","properties":{"sessionID":"ses_1","callID":"c1","tool":"bash","error":"exit 1"}}`)
	evs = ToTranscript(failed)
	if len(evs) != 1 || !evs[0].IsError {
		t.Fatalf("tool.failed -> %+v", evs)
	}
	if !strings.Contains(evs[0].ToolOutput, "exit 1") {
		t.Errorf("error text lost: %q", evs[0].ToolOutput)
	}
}

func TestTextAndReasoningEvents(t *testing.T) {
	text := event(t, `{"type":"session.next.text.ended","properties":{"sessionID":"ses_1","text":"here is the plan"}}`)
	evs := ToTranscript(text)
	if len(evs) != 1 || evs[0].Kind != protocol.EventAssistantMessage {
		t.Fatalf("text.ended -> %+v", evs)
	}

	reasoning := event(t, `{"type":"session.next.reasoning.ended","properties":{"sessionID":"ses_1","text":"thinking it through"}}`)
	evs = ToTranscript(reasoning)
	if len(evs) != 1 || evs[0].Kind != protocol.EventThinking {
		t.Fatalf("reasoning.ended -> %+v", evs)
	}

	empty := event(t, `{"type":"session.next.text.ended","properties":{"sessionID":"ses_1","text":"   "}}`)
	if evs := ToTranscript(empty); len(evs) != 0 {
		t.Errorf("blank text should produce no event, got %+v", evs)
	}
}

func TestParsePermission(t *testing.T) {
	e := event(t, `{"type":"permission.asked","properties":{"id":"per_1","sessionID":"ses_1","permission":"bash","patterns":["rm -rf *"],"metadata":{"command":"rm -rf build"},"always":["bash"],"tool":{"messageID":"m1","callID":"c1"}}}`)
	p, ok := ParsePermission(e)
	if !ok {
		t.Fatal("permission not parsed")
	}
	a := p.ToApproval("mux-session", time.Minute)
	if a.Source != protocol.ApprovalNative {
		t.Errorf("source = %s, want native", a.Source)
	}
	if a.ID != "per_1" {
		t.Errorf("id = %q; OpenCode's request id must be preserved for the reply", a.ID)
	}
	if !strings.Contains(a.Subject, "rm -rf") {
		t.Errorf("subject should name what is being asked: %q", a.Subject)
	}
	if a.ToolInput["command"] != "rm -rf build" {
		t.Errorf("metadata lost: %v", a.ToolInput)
	}
}

// OpenCode accepts once | always | reject. muxscale must never send "always":
// that persists a broadened permission rule, and muxscale relays one answer to
// one question rather than changing a user's policy on their behalf. Same
// reason Claude Code's `rule` is never populated.
func TestApprovalNeverOffersOrSendsAlways(t *testing.T) {
	e := event(t, `{"type":"permission.asked","properties":{"id":"per_1","sessionID":"ses_1","permission":"bash","patterns":[],"metadata":{},"always":["bash"]}}`)
	p, _ := ParsePermission(e)
	a := p.ToApproval("s", time.Minute)

	for _, opt := range a.Options {
		if opt == "always" {
			t.Error(`"always" must not be offered as an option`)
		}
	}
	if len(a.Options) != 2 {
		t.Errorf("options = %v, want exactly allow and deny", a.Options)
	}

	if got := ReplyFor(protocol.ApprovalDecision{Behavior: "allow"}); got != ReplyOnce {
		t.Errorf("allow -> %q, want once (never always)", got)
	}
	if got := ReplyFor(protocol.ApprovalDecision{Behavior: "deny"}); got != ReplyReject {
		t.Errorf("deny -> %q, want reject", got)
	}
	// Anything unrecognised must fail closed, not grant.
	if got := ReplyFor(protocol.ApprovalDecision{Behavior: ""}); got != ReplyReject {
		t.Errorf("an empty decision -> %q, want reject", got)
	}
}

func TestSessionParentIsIdentifiable(t *testing.T) {
	if (Session{}).IsChild() {
		t.Error("a root session was reported as a child")
	}
	if !(Session{ParentID: "ses_parent"}).IsChild() {
		t.Error("a child session was not identified")
	}
}

// ---- client, against a stub server shaped like the real OpenAPI ----

func stubServer(t *testing.T, record func(path string, body []byte)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		// Shaped like the real server: data envelope, location.directory, model.id.
		_, _ = w.Write([]byte(`{"data":[{"id":"ses_1","title":"work","location":{"directory":"/repo"},"agent":"build","model":{"id":"m","providerID":"p"}}]}`))
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(body)
		}
		if record != nil {
			record(r.URL.Path, body)
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestClientHealthAndSessions(t *testing.T) {
	srv := stubServer(t, nil)
	c := NewClient(srv.URL)
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	sessions, err := c.Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "ses_1" || sessions[0].Cwd() != "/repo" {
		t.Errorf("sessions = %+v", sessions)
	}
	if sessions[0].ModelName() != "m" {
		t.Errorf("model = %q", sessions[0].ModelName())
	}
}

// The endpoint has returned both a bare array and an envelope across versions.
func TestSessionsAcceptsBothResponseShapes(t *testing.T) {
	for _, body := range []string{
		`[{"id":"ses_1"}]`,
		`{"sessions":[{"id":"ses_1"}]}`,
		`{"data":[{"id":"ses_1"}]}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		got, err := NewClient(srv.URL).Sessions(context.Background())
		srv.Close()
		if err != nil {
			t.Errorf("%s: %v", body, err)
			continue
		}
		if len(got) != 1 || got[0].ID != "ses_1" {
			t.Errorf("%s -> %+v", body, got)
		}
	}
}

func TestPermissionReplyPostsCorrectBody(t *testing.T) {
	var gotPath string
	var gotBody []byte
	srv := stubServer(t, func(p string, b []byte) { gotPath, gotBody = p, b })

	err := NewClient(srv.URL).ReplyPermission(context.Background(),
		"ses_1", "per_1", ReplyReject, "denied from muxscale")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/session/ses_1/permission/per_1/reply" {
		t.Errorf("path = %q", gotPath)
	}
	var payload map[string]any
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("body %q: %v", gotBody, err)
	}
	if payload["reply"] != "reject" {
		t.Errorf("reply = %v", payload["reply"])
	}
}

// Mid-turn input is the agent's problem to sequence. "queue" is the default
// because interrupting work the user cannot see is the more surprising outcome.
func TestPromptDeliveryDefaultsToQueue(t *testing.T) {
	var gotBody []byte
	srv := stubServer(t, func(_ string, b []byte) { gotBody = b })

	if err := NewClient(srv.URL).Prompt(context.Background(), "ses_1", "carry on", ""); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(gotBody, &payload)
	if payload["delivery"] != "queue" {
		t.Errorf("delivery = %v, want queue", payload["delivery"])
	}
	if payload["prompt"] != "carry on" {
		t.Errorf("prompt = %v", payload["prompt"])
	}
}

func TestPostSurfacesServerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "session is gone", http.StatusNotFound)
	}))
	defer srv.Close()
	err := NewClient(srv.URL).Prompt(context.Background(), "ses_x", "hi", "queue")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "session is gone") {
		t.Errorf("error should carry the server's message, got: %v", err)
	}
}

// Heartbeat comment lines hold the connection open and are not events.
func TestStreamEventsIgnoresHeartbeats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			"data: {\"id\":\"evt_1\",\"type\":\"server.connected\",\"data\":{}}\n" +
				": heartbeat\n\n" +
				"data: {\"id\":\"evt_2\",\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"ses_1\"}}\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	var mu sync.Mutex
	var seen []string
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = StreamEvents(ctx, srv.URL, func(e Event) {
		mu.Lock()
		seen = append(seen, e.Type)
		mu.Unlock()
	})

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "server.connected" || seen[1] != "session.idle" {
		t.Errorf("events = %v; heartbeats must be skipped without dropping real events", seen)
	}
}

func TestOpenCodeUsesOnlyExistingProtocolKinds(t *testing.T) {
	known := map[protocol.EventKind]bool{
		protocol.EventUserMessage: true, protocol.EventAssistantMessage: true,
		protocol.EventThinking: true, protocol.EventToolCall: true,
		protocol.EventToolResult: true, protocol.EventCompaction: true,
		protocol.EventError: true,
	}
	samples := []string{
		`{"type":"session.next.tool.called","properties":{"sessionID":"s","callID":"c","tool":"bash","input":{}}}`,
		`{"type":"session.next.tool.success","properties":{"sessionID":"s","callID":"c","tool":"bash","output":"ok"}}`,
		`{"type":"session.next.text.ended","properties":{"sessionID":"s","text":"hi"}}`,
		`{"type":"session.next.reasoning.ended","properties":{"sessionID":"s","text":"hmm"}}`,
		`{"type":"session.compacted","properties":{"sessionID":"s"}}`,
		`{"type":"session.error","properties":{"sessionID":"s"}}`,
	}
	for _, s := range samples {
		for _, e := range ToTranscript(event(t, s)) {
			if !known[e.Kind] {
				t.Errorf("opencode produced %q, which is not in the shared protocol union", e.Kind)
			}
		}
	}
}

// Regression: the OpenAPI document and the running server disagree on
// opencode 1.18.11. The document advertises `directory`, `path`, `slug` and
// `version`; the server returns those as null and puts the working directory
// in an undocumented `location` object, and sends `model.id` where the
// document says `model.modelID`. A struct that matches the docs but not the
// wire fails silently -- every session would show a blank directory.
func TestFieldsFollowTheWireNotTheDocs(t *testing.T) {
	observed := `{"id":"ses_1","parentID":"","projectID":"p1","agent":"explore",
	  "location":{"directory":"/Users/sam/dev/shop"},
	  "model":{"id":"kimi-k3","providerID":"opencode-go","variant":"default"},
	  "tokens":{"input":193848,"output":14777,"reasoning":1904,"cache":{"read":2452736,"write":0}},
	  "cost":1.5675,"time":{"created":1785723682149,"updated":1785724302914},
	  "title":"Explore shop-web tool architecture (@explore subagent)"}`
	var s Session
	if err := json.Unmarshal([]byte(observed), &s); err != nil {
		t.Fatal(err)
	}
	if s.Cwd() != "/Users/sam/dev/shop" {
		t.Errorf("Cwd() = %q; the directory lives in location.directory on the wire", s.Cwd())
	}
	if s.ModelName() != "kimi-k3" {
		t.Errorf("ModelName() = %q; the server sends model.id", s.ModelName())
	}
	if s.Tokens.Input != 193848 || s.Tokens.Cache.Read != 2452736 {
		t.Errorf("tokens = %+v", s.Tokens)
	}
	if s.UpdatedAt().IsZero() {
		t.Error("time.updated was not decoded")
	}

	// The documented shape must still work, for versions that populate it.
	var doc Session
	if err := json.Unmarshal([]byte(`{"id":"s","directory":"/repo","model":{"modelID":"m"}}`), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Cwd() != "/repo" || doc.ModelName() != "m" {
		t.Errorf("documented shape broke: cwd=%q model=%q", doc.Cwd(), doc.ModelName())
	}
}
