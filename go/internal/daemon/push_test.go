package daemon

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
	"github.com/muxscale/muxscale/go/internal/push"
	"github.com/muxscale/muxscale/go/internal/store"
)

type fakeNotary struct {
	mu   sync.Mutex
	sent []push.Push
	err  error
}

func (f *fakeNotary) Send(p push.Push) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, p)
	return f.err
}

func (f *fakeNotary) all() []push.Push {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]push.Push(nil), f.sent...)
}

type pushHarness struct {
	n      *notifier
	reg    *Registry
	st     *store.Store
	notary *fakeNotary
	key    *ecdh.PrivateKey
}

func newPushHarness(t *testing.T) *pushHarness {
	t.Helper()
	old := pushSettle
	pushSettle = 20 * time.Millisecond
	t.Cleanup(func() { pushSettle = old })
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg := NewRegistry()
	f := &fakeNotary{}
	n := newNotifier(reg, st, f)
	n.machine = "studio"
	key, _ := ecdh.X25519().GenerateKey(rand.Reader)
	if err := st.PutPushDevice(store.PushDevice{
		DeviceID: "phone-1", HostRef: "host-a", Capability: "pc_1", PublicKey: key.PublicKey().Bytes(), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	reg.Insert(protocol.Session{ID: "s1", AgentID: "claude-code", Name: "Rate-limit signup", State: protocol.StateRunning})
	return &pushHarness{n: n, reg: reg, st: st, notary: f, key: key}
}

func (h *pushHarness) opened(t *testing.T, p push.Push) push.Content {
	t.Helper()
	raw, err := push.Open(h.key, p.Sealed)
	if err != nil {
		t.Fatal(err)
	}
	var c push.Content
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never held")
}

func ask(h *pushHarness) *protocol.ApprovalRequest {
	a := &protocol.ApprovalRequest{ID: "ap1", SessionID: "s1", Kind: "tool_use", ToolName: "Bash",
		Subject: "Run the auth tests", ToolInput: map[string]any{"command": "pnpm test"}, PromptID: "p1"}
	h.reg.AskApproval(a)
	h.n.consider(Change{Kind: ChangeApprovalAsk, Approval: a})
	return a
}

func TestAnApprovalReachesThePhoneSealed(t *testing.T) {
	h := newPushHarness(t)
	ask(h)
	eventually(t, func() bool { return len(h.notary.all()) == 1 })
	p := h.notary.all()[0]
	if p.Capability != "pc_1" || p.Kind != "approval" || p.CollapseID != push.CollapseID("host-a", "s1") || p.ThreadID != push.ThreadID("host-a") {
		t.Errorf("push %+v", p)
	}
	if strings.Contains(p.Sealed, "pnpm") {
		t.Error("the command is readable outside the seal")
	}
	c := h.opened(t, p)
	if c.ApprovalID != "ap1" || c.Host != "host-a" || c.Machine != "studio" || c.Tool != "Bash" ||
		c.Name != "Rate-limit signup" || c.PromptID != "p1" || !strings.Contains(c.Detail, "pnpm test") {
		t.Errorf("content %+v", c)
	}
}

func TestAnApprovalAnsweredAtTheKeyboardStaysQuiet(t *testing.T) {
	h := newPushHarness(t)
	a := ask(h)
	h.reg.ResolveApproval(a.ID, protocol.ApprovalDecision{Behavior: "allow"})
	time.Sleep(3 * pushSettle)
	if n := len(h.notary.all()); n != 0 {
		t.Fatalf("%d sent for an approval already answered", n)
	}
}

func TestAFinishedTurnSendsTheLastWords(t *testing.T) {
	h := newPushHarness(t)
	if err := h.st.AppendEvents([]protocol.TranscriptEvent{
		{SessionID: "s1", Seq: 1, Kind: protocol.EventAssistantMessage, Text: "Done: 5 a minute per IP."},
	}); err != nil {
		t.Fatal(err)
	}
	since := time.Now()
	s := h.reg.Update("s1", func(s *protocol.Session) {
		s.State, s.StateSince = protocol.StateWaitingInput, since
		s.Guess = protocol.StateGuess{State: protocol.StateWaitingInput, Confidence: 1}
	})
	h.n.consider(Change{Kind: ChangeStateChange, Session: s, Prev: protocol.StateRunning})
	eventually(t, func() bool { return len(h.notary.all()) == 1 })
	c := h.opened(t, h.notary.all()[0])
	if c.Kind != "waiting" || c.Body != "Done: 5 a minute per IP." {
		t.Errorf("content %+v", c)
	}
}

func TestAGuessedWaitDoesntNotify(t *testing.T) {
	h := newPushHarness(t)
	s := h.reg.Update("s1", func(s *protocol.Session) {
		s.State, s.StateSince = protocol.StateWaitingInput, time.Now()
		s.Guess = protocol.StateGuess{State: protocol.StateWaitingInput, Confidence: 0.5}
	})
	h.n.consider(Change{Kind: ChangeStateChange, Session: s, Prev: protocol.StateRunning})
	time.Sleep(3 * pushSettle)
	if len(h.notary.all()) != 0 {
		t.Fatal("notified on a low-confidence guess")
	}
	h.n.mu.Lock()
	defer h.n.mu.Unlock()
	if len(h.n.recent) != 1 || !strings.Contains(h.n.recent[0].Outcome, "confidence") {
		t.Errorf("recent %+v", h.n.recent)
	}
}

func TestAGonePhoneIsForgotten(t *testing.T) {
	h := newPushHarness(t)
	h.notary.err = push.ErrGone
	ask(h)
	eventually(t, func() bool {
		d, _ := h.st.PushDevices()
		return len(d) == 0
	})
}

func TestQuietHoursHoldBack(t *testing.T) {
	h := newPushHarness(t)
	now := time.Now()
	start := now.Add(-time.Hour).Format("15:04")
	end := now.Add(time.Hour).Format("15:04")
	devs, _ := h.st.PushDevices()
	devs[0].QuietStart, devs[0].QuietEnd = start, end
	h.st.PutPushDevice(devs[0])
	ask(h)
	time.Sleep(3 * pushSettle)
	if len(h.notary.all()) != 0 {
		t.Fatal("sent during quiet hours")
	}
}

func TestLookingAtASessionMutesIt(t *testing.T) {
	h := newPushHarness(t)
	h.n.lookedAt("s1")
	ask(h)
	time.Sleep(3 * pushSettle)
	if len(h.notary.all()) != 0 {
		t.Fatal("notified about a session a phone has open")
	}
}

func TestRegisteringAPhone(t *testing.T) {
	d, mux := authDaemon(t)
	d.push = newNotifier(d.reg, d.store, &fakeNotary{})
	key, _ := ecdh.X25519().GenerateKey(rand.Reader)
	put := func(body string) int {
		req := httptest.NewRequest(http.MethodPut, "/v1/push/devices", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	good := `{"device_id":"phone-1","host_ref":"h","capability":"pc_abc","public_key":"` +
		base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()) + `"}`
	if code := put(good); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	for _, bad := range []string{
		`{"device_id":"phone-1","host_ref":"h","capability":"pc_abc","public_key":"c2hvcnQ="}`,
		`{"device_id":"phone-1","host_ref":"h","capability":"nope","public_key":"` + base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()) + `"}`,
		`not json`,
	} {
		if code := put(bad); code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d", bad, code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/push/devices", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "pc_abc") {
		t.Error("GET shows the capability")
	}
	if !strings.Contains(rec.Body.String(), `"device_id":"phone-1"`) {
		t.Errorf("GET = %s", rec.Body.String())
	}
}
