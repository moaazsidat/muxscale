// Package push builds and dispatches the notification that is the whole point
// of muxscale: your agent blocked, and your phone tells you.
//
// The cloud is not optional here, and it is worth being exact about why. An
// APNs push originates only from a server holding the developer's signing key,
// and that key cannot be distributed to every user's laptop. FCM is the same.
// There is no peer-to-peer path that wakes a backgrounded iOS app. So a "push
// notary" exists: an authenticated endpoint that takes an already-sealed
// payload and forwards it. It is the one piece of cloud that cannot be removed.
//
// What it must never become is a reader. The daemon seals the useful content to
// the device's key and sends the notary only enough plaintext to render a
// lock-screen line at the user's chosen detail level. A notification service
// extension on the device decrypts the rest locally.
package push

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Detail is how much plaintext the user permits the cloud to see.
//
// This is the one genuine information leak in the design -- a lock screen has
// to render before the app runs -- so it is a user-facing setting with the
// private option as the default.
type Detail string

const (
	// DetailSilent: the cloud learns only that something needs attention.
	DetailSilent Detail = "silent"
	// DetailMinimal: agent and host, no session or content. The default.
	DetailMinimal Detail = "minimal"
	// DetailDetailed: adds the session name and the blocking question. Rendered
	// on-device from the sealed blob; the cloud copy stays at minimal.
	DetailDetailed Detail = "detailed"
	// DetailFull: adds a redacted excerpt. Opt-in, with the warning that this
	// text transits Apple and Google infrastructure.
	DetailFull Detail = "full"
)

// Valid reports whether a detail level is recognised.
func (d Detail) Valid() bool {
	switch d {
	case DetailSilent, DetailMinimal, DetailDetailed, DetailFull:
		return true
	}
	return false
}

// Kind classifies a notification, which drives its channel and priority.
type Kind string

const (
	KindApproval  Kind = "approval"
	KindWaiting   Kind = "waiting"
	KindCompleted Kind = "completed"
	KindReminder  Kind = "reminder"
)

// Alert is the cleartext the cloud and the push provider can read.
type Alert struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Notification is what the daemon hands to the notary.
type Notification struct {
	HostKey   string   `json:"host_key"`
	DeviceIDs []string `json:"device_ids"`
	SessionID string   `json:"session_id"`
	Kind      Kind     `json:"kind"`

	// CollapseKey replaces an earlier notification for the same session rather
	// than stacking, so a chatty session does not bury everything else.
	CollapseKey string `json:"collapse_key"`
	Priority    string `json:"priority"`

	// Alert is the plaintext the cloud sees. Never richer than minimal.
	Alert Alert `json:"alert"`
	// Sealed is the real content, encrypted to each device. The cloud cannot
	// read it; the device's notification extension can.
	Sealed map[string]string `json:"sealed,omitempty"`

	// RequiresUnlock marks actions that must not be answerable from a locked
	// screen. A lock-screen tap must never be able to authorize `rm -rf`.
	RequiresUnlock bool `json:"requires_unlock"`
}

// Context is what the daemon knows when a session blocks.
type Context struct {
	AgentID     string
	HostName    string
	SessionID   string
	SessionName string
	Cwd         string
	State       protocol.SessionState
	Approval    *protocol.ApprovalRequest
	Excerpt     string
}

// Build renders a notification at the given detail level.
//
// The invariant this function exists to hold: Alert never contains more than
// the level permits, and anything richer goes in Sealed. A test asserts that
// session names, commands and excerpts cannot appear in the cloud-visible
// alert at minimal or below.
func Build(ctx Context, detail Detail, devices []string, hostKey string) (Notification, error) {
	if len(devices) == 0 {
		return Notification{}, errors.New("no devices to notify")
	}
	if !detail.Valid() {
		return Notification{}, fmt.Errorf("unknown detail level %q", detail)
	}

	kind := KindWaiting
	requiresUnlock := false
	switch {
	case ctx.Approval != nil:
		kind = KindApproval
		// Approving a tool call is a consequential action, so it must not be
		// answerable without unlocking the device.
		requiresUnlock = true
	case ctx.State.Terminal():
		kind = KindCompleted
	}

	n := Notification{
		HostKey:        hostKey,
		DeviceIDs:      devices,
		SessionID:      ctx.SessionID,
		Kind:           kind,
		CollapseKey:    ctx.SessionID,
		Priority:       "high",
		RequiresUnlock: requiresUnlock,
		Alert:          buildAlert(ctx, detail, kind),
	}
	return n, nil
}

// buildAlert renders the cloud-visible text for a detail level.
func buildAlert(ctx Context, detail Detail, kind Kind) Alert {
	switch detail {
	case DetailSilent:
		// Deliberately says nothing at all about which agent or which machine.
		return Alert{Title: "muxscale", Body: "An agent needs you"}

	case DetailMinimal:
		agent := displayAgent(ctx.AgentID)
		title := agent
		if ctx.HostName != "" {
			title = agent + " · " + ctx.HostName
		}
		return Alert{Title: title, Body: minimalBody(kind)}

	case DetailDetailed, DetailFull:
		// Even here the *cloud* copy stays at minimal. The richer text is
		// sealed and rendered on-device by the notification extension, so
		// choosing a higher detail level does not hand more to the cloud.
		agent := displayAgent(ctx.AgentID)
		title := agent
		if ctx.HostName != "" {
			title = agent + " · " + ctx.HostName
		}
		return Alert{Title: title, Body: minimalBody(kind)}
	}
	return Alert{Title: "muxscale", Body: "An agent needs you"}
}

func minimalBody(kind Kind) string {
	switch kind {
	case KindApproval:
		return "Needs your approval"
	case KindCompleted:
		return "Finished"
	case KindReminder:
		return "Still waiting"
	default:
		return "Waiting for you"
	}
}

// SealedPayload is the rich content, encrypted per device.
//
// This is what the notification extension decrypts to render a useful lock
// screen without the cloud ever seeing it.
type SealedPayload struct {
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name,omitempty"`
	Agent       string `json:"agent"`
	Host        string `json:"host,omitempty"`
	Cwd         string `json:"cwd,omitempty"`
	Kind        Kind   `json:"kind"`

	// Question is the blocking prompt or tool call, at detailed and above.
	Question string `json:"question,omitempty"`
	// ToolName and ToolArgs carry the literal arguments, never the agent's
	// description of them.
	ToolName string `json:"tool_name,omitempty"`
	ToolArgs string `json:"tool_args,omitempty"`
	// ApprovalID lets a reply correlate back to the held request.
	ApprovalID string `json:"approval_id,omitempty"`
	// Excerpt is only ever populated at DetailFull.
	Excerpt string `json:"excerpt,omitempty"`
}

// BuildPayload assembles the content to seal for a device.
func BuildPayload(ctx Context, detail Detail) SealedPayload {
	p := SealedPayload{
		SessionID: ctx.SessionID,
		Agent:     displayAgent(ctx.AgentID),
		Host:      ctx.HostName,
		Kind:      KindWaiting,
	}
	if ctx.Approval != nil {
		p.Kind = KindApproval
	} else if ctx.State.Terminal() {
		p.Kind = KindCompleted
	}

	// Silent means silent: nothing beyond the fact that something happened.
	if detail == DetailSilent {
		return SealedPayload{SessionID: ctx.SessionID, Kind: p.Kind}
	}

	p.SessionName = ctx.SessionName
	p.Cwd = ctx.Cwd

	if detail == DetailMinimal {
		return p
	}

	if ctx.Approval != nil {
		p.Question = ctx.Approval.Subject
		p.ToolName = ctx.Approval.ToolName
		p.ApprovalID = ctx.Approval.ID
		p.ToolArgs = formatArgs(ctx.Approval.ToolInput)
	}
	if detail == DetailFull {
		p.Excerpt = ctx.Excerpt
	}
	return p
}

func formatArgs(in map[string]any) string {
	if len(in) == 0 {
		return ""
	}
	parts := make([]string, 0, len(in))
	for k, v := range in {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	// Sorted for stability so the same call always renders the same way.
	sortStrings(parts)
	return strings.Join(parts, " ")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func displayAgent(id string) string {
	switch id {
	case "claude-code":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "opencode":
		return "OpenCode"
	case "pi":
		return "pi"
	case "hermes":
		return "Hermes"
	case "":
		return "Agent"
	default:
		return id
	}
}

// ---- gating ----

// QuietHours suppresses notifications during a nightly window.
type QuietHours struct {
	Enabled bool
	Start   string // "23:00"
	End     string // "07:30"
}

// Active reports whether now falls inside the quiet window.
//
// Handles the window crossing midnight, which is the common case and the easy
// one to get wrong.
func (q QuietHours) Active(now time.Time) bool {
	if !q.Enabled || q.Start == "" || q.End == "" {
		return false
	}
	start, ok1 := parseHM(q.Start)
	end, ok2 := parseHM(q.End)
	if !ok1 || !ok2 {
		return false
	}
	cur := now.Hour()*60 + now.Minute()
	if start == end {
		return false
	}
	if start < end {
		return cur >= start && cur < end
	}
	// Crosses midnight.
	return cur >= start || cur < end
}

func parseHM(s string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// Policy decides whether a notification should be sent at all.
type Policy struct {
	Threshold            float64
	QuietHours           QuietHours
	PerSessionPerMinute  int
	PerHostPer5Min       int
	SuppressForegrounded bool

	mu           sync.Mutex
	sessionSeen  map[string][]time.Time
	hostSeen     []time.Time
	foregrounded map[string]time.Time
}

// NewPolicy builds a policy with the spec's defaults.
func NewPolicy() *Policy {
	return &Policy{
		Threshold:            0.75,
		PerSessionPerMinute:  1,
		PerHostPer5Min:       10,
		SuppressForegrounded: true,
		sessionSeen:          map[string][]time.Time{},
		foregrounded:         map[string]time.Time{},
	}
}

// NoteForegrounded records that a client is actively looking at a session.
func (p *Policy) NoteForegrounded(sessionID string, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.foregrounded[sessionID] = at
}

// Decision explains whether to send, and why not when the answer is no.
type Decision struct {
	Send   bool
	Reason string
}

// foregroundWindow is how recently a client must have been looking at a
// session for a push to be considered redundant.
const foregroundWindow = 20 * time.Second

// Allow decides whether to notify.
//
// Every refusal carries a reason. A notification that silently did not arrive
// is indistinguishable from a bug, and `muxscale debug` needs to be able to
// say which rule suppressed it.
func (p *Policy) Allow(sessionID string, confidence float64, now time.Time) Decision {
	p.mu.Lock()
	defer p.mu.Unlock()

	if confidence < p.Threshold {
		return Decision{false, fmt.Sprintf(
			"confidence %.2f is below the %.2f threshold", confidence, p.Threshold)}
	}
	if p.QuietHours.Active(now) {
		return Decision{false, fmt.Sprintf("quiet hours (%s-%s)",
			p.QuietHours.Start, p.QuietHours.End)}
	}
	if p.SuppressForegrounded {
		if seen, ok := p.foregrounded[sessionID]; ok && now.Sub(seen) < foregroundWindow {
			return Decision{false, "a client is already looking at this session"}
		}
	}

	// Per-session rate limit.
	recent := prune(p.sessionSeen[sessionID], now, time.Minute)
	if p.PerSessionPerMinute > 0 && len(recent) >= p.PerSessionPerMinute {
		p.sessionSeen[sessionID] = recent
		return Decision{false, "per-session rate limit"}
	}
	// Per-host rate limit.
	hostRecent := prune(p.hostSeen, now, 5*time.Minute)
	if p.PerHostPer5Min > 0 && len(hostRecent) >= p.PerHostPer5Min {
		p.hostSeen = hostRecent
		return Decision{false, "per-host rate limit"}
	}

	p.sessionSeen[sessionID] = append(recent, now)
	p.hostSeen = append(hostRecent, now)
	return Decision{true, "sent"}
}

func prune(times []time.Time, now time.Time, window time.Duration) []time.Time {
	out := times[:0]
	for _, t := range times {
		if now.Sub(t) < window {
			out = append(out, t)
		}
	}
	return out
}

// ---- transport ----

// Token is a device's push registration.
type Token struct {
	DeviceID string
	Provider string // "apns" | "fcm"
	Token    string
	Env      string // "production" | "sandbox"
}

// Result is the outcome of one delivery attempt.
type Result struct {
	DeviceID string
	Provider string
	OK       bool
	// Unregistered means the token is dead and should be pruned. APNs says 410,
	// FCM says UNREGISTERED; both mean the same thing and both must lead to the
	// token being removed rather than retried forever.
	Unregistered bool
	Err          error
}

// Transport delivers to a push provider.
//
// This interface is the seam that makes the notary testable without Apple or
// Google credentials. The real implementations are thin: sign a JWT, POST to
// APNs over HTTP/2, or POST to FCM v1. Everything interesting -- detail levels,
// sealing, gating, token pruning -- lives above it and is tested here.
type Transport interface {
	Send(n Notification, tok Token) Result
}

// Dispatcher sends a notification to a set of device tokens.
type Dispatcher struct {
	Transports map[string]Transport // provider -> transport
	OnDead     func(Token)          // called when a token should be pruned
}

// Dispatch delivers to every token, returning per-device results.
func (d *Dispatcher) Dispatch(n Notification, tokens []Token) []Result {
	results := make([]Result, 0, len(tokens))
	for _, tok := range tokens {
		t, ok := d.Transports[tok.Provider]
		if !ok {
			results = append(results, Result{
				DeviceID: tok.DeviceID, Provider: tok.Provider,
				Err: fmt.Errorf("no transport for provider %q", tok.Provider),
			})
			continue
		}
		res := t.Send(n, tok)
		res.DeviceID, res.Provider = tok.DeviceID, tok.Provider
		if res.Unregistered && d.OnDead != nil {
			d.OnDead(tok)
		}
		results = append(results, res)
	}
	return results
}

// Delivered counts successful results.
func Delivered(results []Result) int {
	var n int
	for _, r := range results {
		if r.OK {
			n++
		}
	}
	return n
}
