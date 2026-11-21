package daemon

// Notifications: when a session needs you, tell the phones that asked.
//
// A phone registers here over the tailnet with the capability the push
// notary gave it and the public key notifications are sealed to. When an
// agent asks for approval, asks a question, or finishes a turn, this seals
// what happened to each phone and hands it to the notary, which can't read
// it. Nothing is sent for a phone that never turned notifications on.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
	"github.com/muxscale/muxscale/go/internal/push"
	"github.com/muxscale/muxscale/go/internal/store"
	"github.com/muxscale/muxscale/go/internal/tsnode"
)

// pushSettle is how long a need waits before it's sent. Someone at the
// keyboard usually answers within it, and then the phone stays quiet.
var pushSettle = 3 * time.Second

// reasonSessionStarted is the wait of an agent that has only just started.
// Nothing finished, so nothing needs the person: a terminal reopening its
// windows resumes every agent in them at once.
const reasonSessionStarted = "session started"

type pushSender interface {
	Send(push.Push) error
}

type notifier struct {
	reg     *Registry
	store   *store.Store
	notary  pushSender
	policy  *push.Policy
	machine string
	now     func() time.Time

	mu     sync.Mutex
	recent []pushRecord
}

// pushRecord is one decision, kept for GET /v1/push: a notification that
// didn't arrive needs a reason someone can read.
type pushRecord struct {
	At      time.Time `json:"at"`
	Session string    `json:"session"`
	Kind    string    `json:"kind"`
	Device  string    `json:"device,omitempty"`
	Outcome string    `json:"outcome"`
}

func newNotifier(reg *Registry, st *store.Store, notary pushSender) *notifier {
	machine, _ := os.Hostname()
	machine = strings.TrimSuffix(machine, ".local")
	if i := strings.IndexByte(machine, '.'); i > 0 {
		machine = machine[:i]
	}
	return &notifier{reg: reg, store: st, notary: notary, policy: push.NewPolicy(), machine: machine, now: time.Now}
}

func (n *notifier) run(ctx context.Context) {
	ch, cancel := n.reg.SubscribeInternal()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case c := <-ch:
			n.consider(c)
		}
	}
}

// consider turns a change into a notification, after pushSettle, if it's
// still a need by then.
func (n *notifier) consider(c Change) {
	switch c.Kind {
	case ChangeApprovalAsk:
		if c.Approval == nil {
			return
		}
		a := *c.Approval
		time.AfterFunc(pushSettle, func() {
			if _, pending := n.reg.Approval(a.ID); !pending {
				return
			}
			sess, ok := n.reg.Get(a.SessionID)
			if !ok {
				return
			}
			n.notify(sess, push.ApprovalContent(sess, a, n.machine), 1)
		})
	case ChangeStateChange:
		if c.Session == nil || c.Session.State != protocol.StateWaitingInput || c.Prev.Waiting() ||
			c.Session.Guess.Reason == reasonSessionStarted {
			return
		}
		id, since := c.Session.ID, c.Session.StateSince
		time.AfterFunc(pushSettle, func() {
			sess, ok := n.reg.Get(id)
			if !ok || sess.State != protocol.StateWaitingInput || !sess.StateSince.Equal(since) {
				return
			}
			n.notify(sess, push.WaitingContent(sess, n.lastWords(id), n.machine), sess.Guess.Confidence)
		})
	}
}

// lastWords is the agent's latest message, for a finished turn.
func (n *notifier) lastWords(sessionID string) string {
	if n.store == nil {
		return ""
	}
	events, _, err := n.store.TranscriptBefore(sessionID, ^uint64(0), 30)
	if err != nil {
		return ""
	}
	for i := len(events) - 1; i >= 0; i-- {
		if e := events[i]; e.Kind == protocol.EventAssistantMessage && strings.TrimSpace(e.Text) != "" {
			return strings.TrimSpace(e.Text)
		}
	}
	return ""
}

func (n *notifier) notify(sess protocol.Session, c push.Content, confidence float64) {
	now := n.now()
	devices, err := n.store.PushDevices()
	if err != nil || len(devices) == 0 {
		return
	}
	if d := n.policy.Allow(sess.ID, confidence, now); !d.Send {
		n.record(pushRecord{At: now, Session: sess.ID, Kind: c.Kind, Outcome: "not sent: " + d.Reason})
		return
	}
	for _, dev := range devices {
		rec := pushRecord{At: now, Session: sess.ID, Kind: c.Kind, Device: dev.DeviceID}
		if (push.QuietHours{Enabled: true, Start: dev.QuietStart, End: dev.QuietEnd}).Active(now) {
			rec.Outcome = "not sent: quiet hours"
			n.record(rec)
			continue
		}
		rec.Outcome = n.sendTo(dev, sess.ID, c)
		n.record(rec)
	}
}

func (n *notifier) sendTo(dev store.PushDevice, sessionID string, c push.Content) string {
	c.Host = dev.HostRef
	body, err := c.Marshal()
	if err != nil {
		return "not sent: " + err.Error()
	}
	sealed, err := push.Seal(dev.PublicKey, body)
	if err != nil {
		return "not sent: " + err.Error()
	}
	err = n.notary.Send(push.Push{
		Capability: dev.Capability,
		Kind:       c.Kind,
		CollapseID: push.CollapseID(dev.HostRef, sessionID),
		ThreadID:   push.ThreadID(dev.HostRef),
		Sealed:     sealed,
	})
	if errors.Is(err, push.ErrGone) {
		_ = n.store.DeletePushDevice(dev.DeviceID)
		return "phone's registration is gone; forgotten until it registers again"
	}
	if err != nil {
		return "failed: " + err.Error()
	}
	return "sent"
}

func (n *notifier) record(r pushRecord) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.recent = append(n.recent, r)
	if len(n.recent) > 20 {
		n.recent = n.recent[len(n.recent)-20:]
	}
}

// lookedAt notes that a client has a session open, so a notification about
// it would only repeat what's on screen.
func (n *notifier) lookedAt(sessionID string) {
	if n != nil {
		n.policy.NoteForegrounded(sessionID, n.now())
	}
}

// pushRoutes: a phone registering, and what's been sent.
func (d *Daemon) pushRoutes(mux *http.ServeMux) {
	d.handle(mux, "/v1/push/devices", func(w http.ResponseWriter, r *http.Request) {
		if d.push == nil {
			http.Error(w, "notifications are off on this machine (muxscale up --no-push)", http.StatusServiceUnavailable)
			return
		}
		switch r.Method {
		case http.MethodPut:
			var body struct {
				DeviceID   string `json:"device_id"`
				HostRef    string `json:"host_ref"`
				Capability string `json:"capability"`
				PublicKey  string `json:"public_key"`
				QuietStart string `json:"quiet_start"`
				QuietEnd   string `json:"quiet_end"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
				http.Error(w, "body must be JSON", http.StatusBadRequest)
				return
			}
			key, err := base64.StdEncoding.DecodeString(body.PublicKey)
			if err != nil || len(key) != 32 {
				http.Error(w, "public_key must be a base64 X25519 public key", http.StatusBadRequest)
				return
			}
			if body.DeviceID == "" || len(body.DeviceID) > 128 || body.HostRef == "" || len(body.HostRef) > 128 ||
				!strings.HasPrefix(body.Capability, "pc_") || len(body.Capability) > 64 {
				http.Error(w, "device_id, host_ref and capability are required", http.StatusBadRequest)
				return
			}
			dev := store.PushDevice{
				DeviceID: body.DeviceID, HostRef: body.HostRef, Capability: body.Capability, PublicKey: key,
				QuietStart: body.QuietStart, QuietEnd: body.QuietEnd, CreatedAt: time.Now(),
			}
			if id, ok := tsnode.IdentityFrom(r); ok {
				dev.Identity = id.LoginName
			}
			if err := d.store.PutPushDevice(dev); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		case http.MethodDelete:
			if err := d.store.DeletePushDevice(r.URL.Query().Get("device_id")); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		case http.MethodGet:
			devices, err := d.store.PushDevices()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			d.push.mu.Lock()
			recent := append([]pushRecord(nil), d.push.recent...)
			d.push.mu.Unlock()
			writeJSON(w, map[string]any{"devices": devices, "recent": recent})
		default:
			http.Error(w, "PUT, DELETE or GET", http.StatusMethodNotAllowed)
		}
	})
}
