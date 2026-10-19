// Package emit turns a re-read conversation into an append-only event stream.
//
// Several agents keep a session in a form that is cheapest to read whole:
// a JSONL log that re-appends a message each time it changes (Gemini CLI), a
// log whose tool results arrive after the call (Copilot CLI, Kimi), a record
// that can be rewound. Their adapters re-parse the session on each poll into
// an ordered list of units -- a message, a tool call and its result -- and
// this package decides what is new.
//
// Two rules make the output safe to store. A unit is passed on only once it
// is final, and only in order: later units wait behind an unfinished one, so
// the transcript never shows a reply before the tool call it followed. And
// each unit is keyed by the agent's own ids, not its position, so a unit that
// disappears (a rewind) or reappears changed (a re-appended message) is
// neither emitted twice nor allowed to shift the sequence numbers of others.
package emit

import "github.com/muxscale/muxscale/go/internal/protocol"

// Unit is one piece of a conversation.
type Unit struct {
	// Key identifies the unit across re-reads: the agent's message or tool
	// call id, with a suffix when one message yields several units.
	Key    string
	Events []protocol.TranscriptEvent
	// Early is how many of Events may go out before the unit is final -- a
	// tool call is worth showing while its result is not yet in.
	Early int
	Final bool
}

// Emitter remembers what has gone out.
type Emitter struct {
	seq  uint64
	done map[string]int
	skip uint64
}

// New returns an emitter that has emitted nothing.
func New() *Emitter { return &Emitter{done: map[string]int{}} }

// SeekEnd makes the next Next treat the first fromSeq events as already
// emitted: a reader resumed after a restart continues the stored transcript
// rather than repeating it.
func (e *Emitter) SeekEnd(fromSeq uint64) {
	e.seq, e.skip = 0, fromSeq
	e.done = map[string]int{}
}

// Next returns the events in units that have not gone out yet, numbered.
func (e *Emitter) Next(units []Unit) []protocol.TranscriptEvent {
	var out []protocol.TranscriptEvent
	for _, u := range units {
		avail := len(u.Events)
		if !u.Final {
			avail = min(u.Early, avail)
		}
		for i := e.done[u.Key]; i < avail; i++ {
			e.done[u.Key] = i + 1
			if e.skip > 0 {
				e.skip--
				e.seq++
				continue
			}
			e.seq++
			ev := u.Events[i]
			ev.Seq = e.seq
			out = append(out, ev)
		}
		if !u.Final {
			break
		}
	}
	return out
}
