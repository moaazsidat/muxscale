package daemon

import (
	"unicode/utf8"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// How much of an event's text is kept, and how much a phone is sent.
//
// Adapters read up to 256 KB of any one text. A message -- what a person
// typed, what the agent said back -- is stored whole up to that, and a phone
// is sent its first excerptBytes, marked truncated; See all fetches the rest
// (GET /v1/sessions/event). Everything else is cut when stored: tool output,
// thinking and the like are rarely wanted whole, and are most of a
// transcript's bytes, so keeping them would grow the store many times over.
const (
	excerptBytes = 4096
	// summaryBytes is for a compaction summary, the one long text a reader
	// catching up on a session wants whole.
	summaryBytes = 32 * 1024
)

// kept is an event as it is stored.
func kept(ev protocol.TranscriptEvent) protocol.TranscriptEvent {
	switch ev.Kind {
	case protocol.EventUserMessage, protocol.EventAssistantMessage:
		return ev
	case protocol.EventCompaction:
		ev.Text, ev.Truncated = clipped(ev.Text, summaryBytes, ev.Truncated)
	default:
		ev.Text, ev.Truncated = clipped(ev.Text, excerptBytes, ev.Truncated)
		ev.ToolOutput, ev.Truncated = clipped(ev.ToolOutput, excerptBytes, ev.Truncated)
	}
	return ev
}

// sent is an event as a client is sent it in a transcript or the event feed.
// What hooks report goes out without being stored, so it is cut here too.
func sent(ev protocol.TranscriptEvent) protocol.TranscriptEvent {
	ev = kept(ev)
	if ev.Kind == protocol.EventUserMessage || ev.Kind == protocol.EventAssistantMessage {
		ev.Text, ev.Truncated = clipped(ev.Text, excerptBytes, ev.Truncated)
	}
	return ev
}

func sentAll(events []protocol.TranscriptEvent) []protocol.TranscriptEvent {
	for i := range events {
		events[i] = sent(events[i])
	}
	return events
}

// clipped cuts s to at most max bytes, on a rune boundary, and says whether
// anything is missing: cut now, or already.
func clipped(s string, max int, already bool) (string, bool) {
	if len(s) <= max {
		return s, already
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
