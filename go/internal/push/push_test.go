package push

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

func approvalContext() Context {
	return Context{
		AgentID:     "claude-code",
		HostName:    "mbp",
		SessionID:   "sess_1",
		SessionName: "Refactor the billing adapter",
		Cwd:         "/Users/sam/dev/secret-project",
		State:       protocol.StateWaitingApproval,
		Approval: &protocol.ApprovalRequest{
			ID:        "appr_1",
			Subject:   "Bash: rm -rf ./build",
			ToolName:  "Bash",
			ToolInput: map[string]any{"command": "rm -rf ./build"},
		},
		Excerpt: "the deploy token is sk-live-abcdef",
	}
}

// The invariant the whole detail-level design exists to hold: the cloud-visible
// alert never carries the session name, the command, the cwd, or an excerpt.
// A leak here would be invisible in normal use and permanent once shipped.
func TestCloudVisibleAlertNeverLeaksContent(t *testing.T) {
	ctx := approvalContext()
	canaries := []string{
		"Refactor the billing adapter", "rm -rf", "secret-project",
		"sk-live-abcdef", "sess_1", "appr_1",
	}

	for _, detail := range []Detail{DetailSilent, DetailMinimal, DetailDetailed, DetailFull} {
		n, err := Build(ctx, detail, []string{"dev_1"}, "hk_1")
		if err != nil {
			t.Fatalf("%s: %v", detail, err)
		}
		visible := n.Alert.Title + " " + n.Alert.Body
		for _, c := range canaries {
			if strings.Contains(visible, c) {
				t.Errorf("detail=%s leaked %q into the cloud-visible alert: %q",
					detail, c, visible)
			}
		}
	}
}

// Choosing a richer detail level must not hand more to the cloud. The extra
// content is sealed and rendered on-device.
func TestHigherDetailDoesNotWidenTheCloudAlert(t *testing.T) {
	ctx := approvalContext()
	minimal, _ := Build(ctx, DetailMinimal, []string{"d"}, "hk")
	full, _ := Build(ctx, DetailFull, []string{"d"}, "hk")

	if minimal.Alert != full.Alert {
		t.Errorf("the cloud alert changed with detail level:\n minimal=%+v\n full=%+v",
			minimal.Alert, full.Alert)
	}
}

// Silent means the cloud learns nothing about which agent or machine.
func TestSilentRevealsNothing(t *testing.T) {
	ctx := approvalContext()
	n, _ := Build(ctx, DetailSilent, []string{"d"}, "hk")
	visible := n.Alert.Title + " " + n.Alert.Body
	for _, c := range []string{"Claude", "mbp", "claude-code"} {
		if strings.Contains(visible, c) {
			t.Errorf("silent detail leaked %q: %q", c, visible)
		}
	}
}

func TestMinimalNamesAgentAndHostOnly(t *testing.T) {
	ctx := approvalContext()
	n, _ := Build(ctx, DetailMinimal, []string{"d"}, "hk")
	if !strings.Contains(n.Alert.Title, "Claude Code") {
		t.Errorf("title should name the agent: %q", n.Alert.Title)
	}
	if !strings.Contains(n.Alert.Title, "mbp") {
		t.Errorf("title should name the host: %q", n.Alert.Title)
	}
	if !strings.Contains(strings.ToLower(n.Alert.Body), "approval") {
		t.Errorf("body should say what is wanted: %q", n.Alert.Body)
	}
}

// A lock-screen tap must never be able to authorize `rm -rf`.
func TestApprovalsRequireUnlock(t *testing.T) {
	n, _ := Build(approvalContext(), DetailMinimal, []string{"d"}, "hk")
	if n.Kind != KindApproval {
		t.Errorf("kind = %s, want approval", n.Kind)
	}
	if !n.RequiresUnlock {
		t.Error("an approval must require device unlock before it can be answered")
	}

	waiting := approvalContext()
	waiting.Approval = nil
	waiting.State = protocol.StateWaitingInput
	w, _ := Build(waiting, DetailMinimal, []string{"d"}, "hk")
	if w.Kind != KindWaiting {
		t.Errorf("kind = %s, want waiting", w.Kind)
	}
	if w.RequiresUnlock {
		t.Error("a plain waiting notification need not require unlock")
	}
}

// A chatty session must not bury everything else on the lock screen.
func TestCollapseKeyIsPerSession(t *testing.T) {
	n, _ := Build(approvalContext(), DetailMinimal, []string{"d"}, "hk")
	if n.CollapseKey != "sess_1" {
		t.Errorf("collapse key = %q, want the session id", n.CollapseKey)
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	if _, err := Build(approvalContext(), DetailMinimal, nil, "hk"); err == nil {
		t.Error("no devices should be an error")
	}
	if _, err := Build(approvalContext(), Detail("loud"), []string{"d"}, "hk"); err == nil {
		t.Error("an unknown detail level should be an error, not silently treated as minimal")
	}
}

// ---- sealed payload ----

func TestSealedPayloadCarriesTheRichContent(t *testing.T) {
	ctx := approvalContext()
	p := BuildPayload(ctx, DetailDetailed)

	if p.SessionName != "Refactor the billing adapter" {
		t.Errorf("session name = %q", p.SessionName)
	}
	if p.ToolName != "Bash" || !strings.Contains(p.ToolArgs, "rm -rf ./build") {
		t.Errorf("the literal tool arguments must be present: %+v", p)
	}
	if p.ApprovalID != "appr_1" {
		t.Errorf("approval id = %q; a reply needs it to correlate", p.ApprovalID)
	}
	// Excerpt is DetailFull only.
	if p.Excerpt != "" {
		t.Errorf("detailed must not include an excerpt: %q", p.Excerpt)
	}

	full := BuildPayload(ctx, DetailFull)
	if full.Excerpt == "" {
		t.Error("full detail should include the excerpt")
	}
}

func TestSealedPayloadRespectsSilent(t *testing.T) {
	p := BuildPayload(approvalContext(), DetailSilent)
	if p.SessionName != "" || p.Question != "" || p.ToolArgs != "" || p.Cwd != "" {
		t.Errorf("silent must seal nothing beyond the bare fact: %+v", p)
	}
	if p.SessionID == "" {
		t.Error("even silent needs the session id to route a tap")
	}
}

func TestMinimalPayloadHasNoQuestion(t *testing.T) {
	p := BuildPayload(approvalContext(), DetailMinimal)
	if p.Question != "" || p.ToolArgs != "" {
		t.Errorf("minimal must not seal the question or arguments: %+v", p)
	}
	if p.SessionName == "" {
		t.Error("minimal should still carry the session name for on-device display")
	}
}

// The literal arguments are what a human needs in order to judge an action, so
// they must render stably rather than in map order.
func TestToolArgsRenderDeterministically(t *testing.T) {
	in := map[string]any{"zebra": 1, "alpha": "two", "middle": true}
	first := formatArgs(in)
	for i := 0; i < 20; i++ {
		if got := formatArgs(in); got != first {
			t.Fatalf("argument rendering is unstable: %q vs %q", first, got)
		}
	}
	if !strings.HasPrefix(first, "alpha=") {
		t.Errorf("arguments should be sorted: %q", first)
	}
}

// ---- gating ----

func TestConfidenceThreshold(t *testing.T) {
	p := NewPolicy()
	now := time.Now()
	if d := p.Allow("s1", 0.5, now); d.Send {
		t.Error("a low-confidence guess must not notify")
	} else if !strings.Contains(d.Reason, "confidence") {
		t.Errorf("the refusal should name the rule: %q", d.Reason)
	}
	if d := p.Allow("s1", 0.9, now); !d.Send {
		t.Errorf("a confident guess should notify: %s", d.Reason)
	}
}

func TestPerSessionRateLimit(t *testing.T) {
	p := NewPolicy()
	now := time.Now()
	if d := p.Allow("s1", 1.0, now); !d.Send {
		t.Fatalf("first should send: %s", d.Reason)
	}
	if d := p.Allow("s1", 1.0, now.Add(time.Second)); d.Send {
		t.Error("a second push within the minute should be suppressed")
	}
	if d := p.Allow("s1", 1.0, now.Add(2*time.Minute)); !d.Send {
		t.Errorf("after the window it should send again: %s", d.Reason)
	}
}

func TestPerHostRateLimit(t *testing.T) {
	p := NewPolicy()
	p.PerSessionPerMinute = 0 // isolate the host limit
	now := time.Now()
	for i := 0; i < p.PerHostPer5Min; i++ {
		if d := p.Allow("s"+string(rune('a'+i)), 1.0, now); !d.Send {
			t.Fatalf("push %d should have sent: %s", i, d.Reason)
		}
	}
	if d := p.Allow("s_extra", 1.0, now); d.Send {
		t.Error("the host limit should have suppressed this")
	} else if !strings.Contains(d.Reason, "host") {
		t.Errorf("reason should name the host limit: %q", d.Reason)
	}
}

// The window crossing midnight is the common case and the easy one to get
// wrong.
func TestQuietHoursAcrossMidnight(t *testing.T) {
	q := QuietHours{Enabled: true, Start: "23:00", End: "07:30"}
	at := func(h, m int) time.Time {
		return time.Date(2026, 8, 11, h, m, 0, 0, time.UTC)
	}
	cases := map[string]struct {
		t    time.Time
		want bool
	}{
		"just before":     {at(22, 59), false},
		"at start":        {at(23, 0), true},
		"after midnight":  {at(2, 0), true},
		"just before end": {at(7, 29), true},
		"at end":          {at(7, 30), false},
		"midday":          {at(12, 0), false},
	}
	for name, c := range cases {
		if got := q.Active(c.t); got != c.want {
			t.Errorf("%s (%s): active = %v, want %v", name, c.t.Format("15:04"), got, c.want)
		}
	}
}

func TestQuietHoursSameDayWindow(t *testing.T) {
	q := QuietHours{Enabled: true, Start: "09:00", End: "17:00"}
	at := func(h int) time.Time { return time.Date(2026, 8, 11, h, 0, 0, 0, time.UTC) }
	if q.Active(at(8)) || !q.Active(at(12)) || q.Active(at(18)) {
		t.Error("a same-day window is wrong")
	}
}

func TestQuietHoursDisabledOrMalformed(t *testing.T) {
	now := time.Now()
	if (QuietHours{Start: "23:00", End: "07:00"}).Active(now) {
		t.Error("a disabled window must never be active")
	}
	if (QuietHours{Enabled: true, Start: "nonsense", End: "07:00"}).Active(now) {
		t.Error("a malformed window should fail open, not suppress everything silently")
	}
	if (QuietHours{Enabled: true, Start: "09:00", End: "09:00"}).Active(now) {
		t.Error("a zero-length window should never be active")
	}
}

func TestQuietHoursSuppresses(t *testing.T) {
	p := NewPolicy()
	p.QuietHours = QuietHours{Enabled: true, Start: "23:00", End: "07:00"}
	night := time.Date(2026, 8, 11, 2, 0, 0, 0, time.UTC)
	d := p.Allow("s1", 1.0, night)
	if d.Send {
		t.Error("quiet hours should suppress")
	}
	if !strings.Contains(d.Reason, "quiet") {
		t.Errorf("reason should name quiet hours: %q", d.Reason)
	}
}

// If the user is already looking at the session, buzzing their phone is noise.
func TestForegroundedSessionSuppresses(t *testing.T) {
	p := NewPolicy()
	now := time.Now()
	p.NoteForegrounded("s1", now)

	if d := p.Allow("s1", 1.0, now.Add(2*time.Second)); d.Send {
		t.Error("a session being watched should not push")
	}
	// But the suppression must expire, or a session viewed once is muted
	// forever.
	if d := p.Allow("s1", 1.0, now.Add(time.Minute)); !d.Send {
		t.Errorf("suppression should lapse: %s", d.Reason)
	}
}

// Every refusal must be explainable. A notification that silently did not
// arrive is indistinguishable from a bug.
func TestEveryRefusalHasAReason(t *testing.T) {
	p := NewPolicy()
	p.QuietHours = QuietHours{Enabled: true, Start: "00:00", End: "23:59"}
	d := p.Allow("s1", 1.0, time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
	if d.Send {
		t.Fatal("expected suppression")
	}
	if d.Reason == "" {
		t.Error("a refusal with no reason is undebuggable")
	}
}

// ---- dispatch ----

type fakeTransport struct {
	sent         []Notification
	fail         bool
	unregistered bool
}

func (f *fakeTransport) Send(n Notification, tok Token) Result {
	f.sent = append(f.sent, n)
	if f.unregistered {
		return Result{Unregistered: true, Err: errors.New("410 gone")}
	}
	if f.fail {
		return Result{Err: errors.New("provider rejected")}
	}
	return Result{OK: true}
}

func TestDispatchToMultipleProviders(t *testing.T) {
	apns := &fakeTransport{}
	fcm := &fakeTransport{}
	d := &Dispatcher{Transports: map[string]Transport{"apns": apns, "fcm": fcm}}

	n, _ := Build(approvalContext(), DetailMinimal, []string{"d1", "d2"}, "hk")
	results := d.Dispatch(n, []Token{
		{DeviceID: "d1", Provider: "apns", Token: "t1"},
		{DeviceID: "d2", Provider: "fcm", Token: "t2"},
	})

	if Delivered(results) != 2 {
		t.Errorf("delivered %d of 2: %+v", Delivered(results), results)
	}
	if len(apns.sent) != 1 || len(fcm.sent) != 1 {
		t.Errorf("each provider should have received one: apns=%d fcm=%d",
			len(apns.sent), len(fcm.sent))
	}
}

// A dead token must be pruned, not retried forever. APNs says 410, FCM says
// UNREGISTERED; both mean the same thing.
func TestDeadTokensArePruned(t *testing.T) {
	var pruned []Token
	d := &Dispatcher{
		Transports: map[string]Transport{"apns": &fakeTransport{unregistered: true}},
		OnDead:     func(tok Token) { pruned = append(pruned, tok) },
	}
	n, _ := Build(approvalContext(), DetailMinimal, []string{"d1"}, "hk")
	results := d.Dispatch(n, []Token{{DeviceID: "d1", Provider: "apns", Token: "dead"}})

	if Delivered(results) != 0 {
		t.Error("a dead token should not count as delivered")
	}
	if len(pruned) != 1 || pruned[0].Token != "dead" {
		t.Errorf("the dead token was not pruned: %+v", pruned)
	}
}

// One device failing must not stop the others: the whole point is reaching
// whichever phone the user actually has.
func TestOneFailureDoesNotStopOthers(t *testing.T) {
	d := &Dispatcher{Transports: map[string]Transport{
		"apns": &fakeTransport{fail: true},
		"fcm":  &fakeTransport{},
	}}
	n, _ := Build(approvalContext(), DetailMinimal, []string{"d1", "d2"}, "hk")
	results := d.Dispatch(n, []Token{
		{DeviceID: "d1", Provider: "apns"},
		{DeviceID: "d2", Provider: "fcm"},
	})
	if len(results) != 2 {
		t.Fatalf("got %d results", len(results))
	}
	if Delivered(results) != 1 {
		t.Errorf("the working provider should still have delivered: %+v", results)
	}
}

func TestUnknownProviderIsReportedNotDropped(t *testing.T) {
	d := &Dispatcher{Transports: map[string]Transport{}}
	n, _ := Build(approvalContext(), DetailMinimal, []string{"d1"}, "hk")
	results := d.Dispatch(n, []Token{{DeviceID: "d1", Provider: "carrier-pigeon"}})
	if len(results) != 1 || results[0].Err == nil {
		t.Errorf("an unknown provider must surface an error: %+v", results)
	}
}

// The notification the cloud receives must be serialisable without the sealed
// content becoming readable.
func TestNotificationSerialisesWithoutLeaking(t *testing.T) {
	n, _ := Build(approvalContext(), DetailFull, []string{"d1"}, "hk")
	n.Sealed = map[string]string{"d1": "BASE64CIPHERTEXT=="}

	b, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(b)
	for _, c := range []string{"rm -rf", "billing adapter", "sk-live"} {
		if strings.Contains(wire, c) {
			t.Errorf("the wire form leaked %q: %s", c, wire)
		}
	}
	if !strings.Contains(wire, "BASE64CIPHERTEXT") {
		t.Error("the sealed blob should be carried")
	}
}
