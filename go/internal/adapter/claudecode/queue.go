package claudecode

import (
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Claude Code keeps its own record of messages typed while it is busy, as
// queue-operation entries in the transcript (verified against 2.1.x files):
//
//	{"type":"queue-operation","operation":"enqueue","content":"…","timestamp":…}
//	{"type":"queue-operation","operation":"dequeue",…}           oldest taken as the next prompt
//	{"type":"queue-operation","operation":"remove","content":"…"} that one taken mid-turn
//	{"type":"queue-operation","operation":"popAll","content":"…"} all pulled back into the editor
//
// Replaying them gives what is still waiting. Claude Code queues its own
// task notifications the same way; they are kept in the replay, so a dequeue
// still takes the right item, and left out of what is shown.

type queued struct {
	msg    protocol.QueuedMessage
	hidden bool
}

// queue replays queue-operation entries.
type queue struct {
	items   []queued
	changed bool
}

func (q *queue) apply(op, content string, at time.Time) {
	switch op {
	case "enqueue":
		q.items = append(q.items, queued{
			msg:    protocol.QueuedMessage{Text: content, Delivery: "queue", At: at},
			hidden: internalQueued(content),
		})
	case "dequeue":
		if len(q.items) > 0 {
			q.items = q.items[1:]
		}
	case "remove":
		for i, it := range q.items {
			if it.msg.Text == content {
				q.items = append(q.items[:i:i], q.items[i+1:]...)
				break
			}
		}
	case "popAll":
		q.items = nil
	default:
		return
	}
	q.changed = true
}

// snapshot is what a person would see waiting, oldest first. Never nil, so an
// emptied queue reads as empty rather than as unknown.
func (q *queue) snapshot() []protocol.QueuedMessage {
	out := []protocol.QueuedMessage{}
	for _, it := range q.items {
		if !it.hidden {
			out = append(out, it.msg)
		}
	}
	return out
}

// internalQueued is Claude Code queueing a note to itself, such as a
// background task finishing, rather than something a person typed.
func internalQueued(content string) bool {
	return strings.HasPrefix(strings.TrimSpace(content), "<task-notification>")
}
