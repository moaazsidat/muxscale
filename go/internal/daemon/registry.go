package daemon

import (
	"crypto/rand"
	"encoding/base32"
	"sort"
	"sync"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Registry holds live session state. M0 keeps this in memory; the SQLite store
// lands in M1. The interface is deliberately narrow so swapping the backing
// store does not ripple into the hook handler.
type Registry struct {
	mu        sync.RWMutex
	sessions  map[string]*protocol.Session // by muxscale id
	byNatural map[string]string            // agentID+"/"+naturalKey -> muxscale id

	// approvals are keyed by approval id and hold the channel a blocked hook
	// handler is waiting on.
	approvals map[string]*pendingApproval

	subs   map[int]chan Change
	nextID int
}

// pendingApproval is a decision hook parked awaiting a human.
type pendingApproval struct {
	req    *protocol.ApprovalRequest
	answer chan protocol.ApprovalDecision
	once   sync.Once
}

// ChangeKind describes what happened.
type ChangeKind string

const (
	ChangeSessionUpsert ChangeKind = "session_upsert"
	// ChangeSessionRemove says a row is gone, folded into another that is
	// the same agent session. Clients drop it; the store forgets it.
	ChangeSessionRemove ChangeKind = "session_remove"
	// ChangeTranscriptReset says a session's transcript is being read again
	// from the start. Clients drop what they hold of it and load it afresh.
	ChangeTranscriptReset ChangeKind = "transcript_reset"
	ChangeStateChange     ChangeKind = "state_change"
	ChangeTranscript      ChangeKind = "transcript"
	ChangeApprovalAsk     ChangeKind = "approval_ask"
	ChangeApprovalResolve ChangeKind = "approval_resolve"
)

// Change is a registry event delivered to subscribers.
type Change struct {
	Kind     ChangeKind                `json:"kind"`
	Session  *protocol.Session         `json:"session,omitempty"`
	Event    *protocol.TranscriptEvent `json:"event,omitempty"`
	Approval *protocol.ApprovalRequest `json:"approval,omitempty"`
	Prev     protocol.SessionState     `json:"prev,omitempty"`
	At       time.Time                 `json:"at"`
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		sessions:  map[string]*protocol.Session{},
		byNatural: map[string]string{},
		approvals: map[string]*pendingApproval{},
		subs:      map[int]chan Change{},
	}
}

// Subscribe returns a channel of changes and a cancel func.
func (r *Registry) Subscribe() (<-chan Change, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextID
	r.nextID++
	// Buffered: a slow subscriber must not stall the hook handler, which is
	// holding an agent's request open.
	ch := make(chan Change, 256)
	r.subs[id] = ch
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if c, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(c)
		}
	}
}

// broadcast must be called with the lock held.
func (r *Registry) broadcast(c Change) {
	c.At = time.Now()
	for _, ch := range r.subs {
		select {
		case ch <- c:
		default: // drop for slow subscribers rather than block
		}
	}
}

// Upsert finds or creates a session by its natural key, applying mutate.
//
// The returned session is a copy, never the registry's own struct. Callers
// marshal it to clients, and a client that read a session while a concurrent
// state change was landing on the same memory was a data race `go test -race`
// caught between this and SetState. Everything a caller needs from the result
// -- the id, mostly -- is in the copy.
func (r *Registry) Upsert(agentID, naturalKey string, mutate func(*protocol.Session)) *protocol.Session {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := agentID + "/" + naturalKey
	id, ok := r.byNatural[key]
	var s *protocol.Session
	if ok {
		s = r.sessions[id]
	} else {
		now := time.Now()
		s = &protocol.Session{
			ID:         newID(),
			AgentID:    agentID,
			NaturalKey: naturalKey,
			State:      protocol.StateStarting,
			StateSince: now,
			CreatedAt:  now,
		}
		r.sessions[s.ID] = s
		r.byNatural[key] = s.ID
	}
	if mutate != nil {
		mutate(s)
	}
	s.LastEventAt = time.Now()
	announced := *s
	r.broadcast(Change{Kind: ChangeSessionUpsert, Session: &announced})
	returned := *s
	return &returned
}

// Insert registers a session under its existing ID, for restoring from disk.
//
// Upsert cannot be used for this: it mints a fresh ID and keys the map by it,
// so overwriting the ID afterwards would leave the map key and the session's
// own ID disagreeing, and every later lookup by that ID would silently miss.
func (r *Registry) Insert(s protocol.Session) *protocol.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.ID == "" {
		s.ID = newID()
	}
	internal := s
	r.sessions[s.ID] = &internal
	r.byNatural[s.AgentID+"/"+s.NaturalKey] = s.ID
	announced := s
	r.broadcast(Change{Kind: ChangeSessionUpsert, Session: &announced})
	returned := s
	return &returned
}

// Update mutates an existing session by ID, returning nil if there is none.
//
// Distinct from Upsert, which keys on the agent's natural key and will happily
// create a session that was never there. Update only ever edits.
func (r *Registry) Update(id string, mutate func(*protocol.Session)) *protocol.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return nil
	}
	if mutate != nil {
		mutate(s)
	}
	s.LastEventAt = time.Now()
	cp := *s
	r.broadcast(Change{Kind: ChangeSessionUpsert, Session: &cp})
	return &cp
}

// Remove deletes a session from the live registry.
//
// Used to reconcile duplicates: the same real agent session can be found twice,
// once by process scan and once by its own hooks, because the two paths key on
// different things. Whichever attachment is weaker gets dropped, so a phone
// shows one row per agent rather than two.
func (r *Registry) Remove(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return false
	}
	delete(r.sessions, id)
	// Every key that led here, aliases included.
	for k, v := range r.byNatural {
		if v == id {
			delete(r.byNatural, k)
		}
	}
	gone := *s
	r.broadcast(Change{Kind: ChangeSessionRemove, Session: &gone})
	return true
}

// ResetTranscript tells clients a session's transcript is being rebuilt.
func (r *Registry) ResetTranscript(id string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[id]
	if !ok {
		return
	}
	cp := *s
	r.broadcast(Change{Kind: ChangeTranscriptReset, Session: &cp})
}

// IDFor returns the session an agent's natural key leads to.
func (r *Registry) IDFor(agentID, naturalKey string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byNatural[agentID+"/"+naturalKey]
	return id, ok
}

// Alias makes a second natural key lead to an existing session, so that two
// ways of finding one agent session -- its process, and its own id -- land on
// the same row instead of making two.
func (r *Registry) Alias(agentID, naturalKey, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sessions[id]; !ok {
		return false
	}
	r.byNatural[agentID+"/"+naturalKey] = id
	return true
}

// FindByTranscript returns sessions following a given transcript file.
func (r *Registry) FindByTranscript(path string) []protocol.Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []protocol.Session
	for _, s := range r.sessions {
		if s.TranscriptPath != "" && s.TranscriptPath == path {
			out = append(out, *s)
		}
	}
	return out
}

// SetState transitions a session, recording the guess that justified it.
// Returns true if the state actually changed.
func (r *Registry) SetState(sessionID string, g protocol.StateGuess) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[sessionID]
	if !ok {
		return false
	}
	prev := s.State
	s.Guess = g
	if prev == g.State {
		return false
	}
	s.State = g.State
	s.StateSince = time.Now()
	s.LastEventAt = s.StateSince
	cp := *s
	r.broadcast(Change{Kind: ChangeStateChange, Session: &cp, Prev: prev})
	return true
}

// AppendEvent records a transcript event.
func (r *Registry) AppendEvent(ev protocol.TranscriptEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.broadcast(Change{Kind: ChangeTranscript, Event: &ev})
}

// NoteOutput records that a session produced output, without retaining it.
// Output activity is the quiescence signal that state inference uses for
// agents with no hooks; the bytes themselves are not stored here.
func (r *Registry) NoteOutput(sessionID string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[sessionID]; ok {
		s.LastEventAt = time.Now()
	}
}

// Get returns a copy of a session.
func (r *Registry) Get(id string) (protocol.Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[id]
	if !ok {
		return protocol.Session{}, false
	}
	return *s, true
}

// List returns all sessions, waiting ones first, then most-recent.
// Blocked sessions belong at the top: that is the whole reason to look.
func (r *Registry) List() []protocol.Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]protocol.Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		wi, wj := out[i].State.Waiting(), out[j].State.Waiting()
		if wi != wj {
			return wi
		}
		ti, tj := out[i].State.Terminal(), out[j].State.Terminal()
		if ti != tj {
			return tj
		}
		return out[i].LastEventAt.After(out[j].LastEventAt)
	})
	return out
}

// AskApproval registers a pending approval and returns the channel to wait on.
func (r *Registry) AskApproval(req *protocol.ApprovalRequest) <-chan protocol.ApprovalDecision {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := &pendingApproval{req: req, answer: make(chan protocol.ApprovalDecision, 1)}
	r.approvals[req.ID] = p
	if s, ok := r.sessions[req.SessionID]; ok {
		s.PendingApproval = req
	}
	cp := *req
	r.broadcast(Change{Kind: ChangeApprovalAsk, Approval: &cp})
	return p.answer
}

// ResolveApproval delivers a human's decision. Safe to call more than once;
// only the first answer wins, which is what makes the local-answer race
// (§4.5.4) resolve cleanly rather than panicking on a double send.
func (r *Registry) ResolveApproval(id string, d protocol.ApprovalDecision) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.approvals[id]
	if !ok {
		return false
	}
	delivered := false
	p.once.Do(func() {
		p.answer <- d
		close(p.answer)
		delivered = true
	})
	delete(r.approvals, id)
	if s, ok := r.sessions[p.req.SessionID]; ok && s.PendingApproval != nil && s.PendingApproval.ID == id {
		s.PendingApproval = nil
	}
	cp := *p.req
	r.broadcast(Change{Kind: ChangeApprovalResolve, Approval: &cp})
	return delivered
}

// Approval returns a pending approval by id.
func (r *Registry) Approval(id string) (protocol.ApprovalRequest, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.approvals[id]
	if !ok {
		return protocol.ApprovalRequest{}, false
	}
	return *p.req, true
}

// SessionApprovals lists one session's unanswered approvals, oldest first.
func (r *Registry) SessionApprovals(sessionID string) []protocol.ApprovalRequest {
	var out []protocol.ApprovalRequest
	for _, a := range r.PendingApprovals() {
		if a.SessionID == sessionID {
			out = append(out, a)
		}
	}
	return out
}

// PendingApprovals lists unanswered approvals, oldest first.
func (r *Registry) PendingApprovals() []protocol.ApprovalRequest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]protocol.ApprovalRequest, 0, len(r.approvals))
	for _, p := range r.approvals {
		out = append(out, *p.req)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AskedAt.Before(out[j].AskedAt) })
	return out
}

var idEnc = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// newID mints a time-ordered, URL-safe session id.
func newID() string {
	var b [10]byte
	ts := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ts)
		ts >>= 8
	}
	_, _ = rand.Read(b[6:])
	return idEnc.EncodeToString(b[:])
}
