package daemon

import (
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// screenQueueEvery is how often a busy session's screen is read for its queue.
// A queue is typed by a person, so a couple of seconds is quick enough, and a
// screen read of an adopted terminal is a call into that terminal.
const screenQueueEvery = 2 * time.Second

// screenQueueLoop reads the queue off the screen for agents that keep theirs
// only in memory (adapter.ScreenQueueReader).
func (m *Manager) screenQueueLoop() {
	t := time.NewTicker(screenQueueEvery)
	defer t.Stop()
	for n := 0; ; n++ {
		<-t.C
		m.readScreenQueues(n%idleQueueEvery == 0)
	}
}

// idleQueueEvery is how many ticks pass between reads of a session that does
// not look busy. Its state can be wrong -- a row the scan found before its
// transcript was joined says waiting while the agent works -- so it is still
// read, only less often.
const idleQueueEvery = 5

// readScreenQueues updates each such session's queue from its screen: a busy
// one, or one already showing a queue so its emptying is seen, every time;
// the rest only when idleToo, since an idle agent takes a message at once.
func (m *Manager) readScreenQueues(idleToo bool) {
	if m.adapters == nil {
		return
	}
	for _, s := range m.reg.List() {
		a, ok := m.adapterByID(s.AgentID)
		if !ok {
			continue
		}
		r, ok := a.(adapter.ScreenQueueReader)
		if !ok {
			continue
		}
		busy := s.State == protocol.StateRunning || s.State == protocol.StateWaitingApproval
		if !busy && len(s.Queued) == 0 && !idleToo {
			continue
		}
		text, ok := m.ScreenText(s.ID)
		if !ok {
			continue // no screen to read: queue unknown, left as it was
		}
		q := r.QueueFromScreen(text)
		if sameQueue(q, s.Queued) {
			continue
		}
		m.reg.Update(s.ID, func(x *protocol.Session) { x.Queued = q })
	}
}

func sameQueue(a, b []protocol.QueuedMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Text != b[i].Text || a[i].Delivery != b[i].Delivery || a[i].Partial != b[i].Partial {
			return false
		}
	}
	return true
}
