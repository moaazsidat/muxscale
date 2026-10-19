package emit

import (
	"testing"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

func ev(text string) protocol.TranscriptEvent {
	return protocol.TranscriptEvent{Kind: protocol.EventAssistantMessage, Text: text}
}

func texts(evs []protocol.TranscriptEvent) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Text)
	}
	return out
}

func TestOrderFinalityAndKeys(t *testing.T) {
	e := New()
	units := []Unit{
		{Key: "u1", Events: []protocol.TranscriptEvent{ev("hello")}, Final: true},
		{Key: "t1", Events: []protocol.TranscriptEvent{ev("call"), ev("result")}, Early: 1},
		{Key: "a1", Events: []protocol.TranscriptEvent{ev("after")}, Final: true},
	}
	got := e.Next(units)
	if want := []string{"hello", "call"}; len(got) != 2 || got[0].Text != want[0] || got[1].Text != want[1] || got[1].Seq != 2 {
		t.Fatalf("first = %v", texts(got))
	}
	// Re-reading the same state emits nothing.
	if again := e.Next(units); len(again) != 0 {
		t.Fatalf("repeated %v", texts(again))
	}
	units[1].Final = true
	got = e.Next(units)
	if len(got) != 2 || got[0].Text != "result" || got[0].Seq != 3 || got[1].Text != "after" || got[1].Seq != 4 {
		t.Fatalf("second = %v", texts(got))
	}

	// A rewind removes a unit; the next one still gets the next number.
	got = e.Next([]Unit{units[0], {Key: "a2", Events: []protocol.TranscriptEvent{ev("new")}, Final: true}})
	if len(got) != 1 || got[0].Text != "new" || got[0].Seq != 5 {
		t.Fatalf("after rewind = %+v", got)
	}
}

func TestSeekEndResumesNumbering(t *testing.T) {
	units := []Unit{
		{Key: "u1", Events: []protocol.TranscriptEvent{ev("a")}, Final: true},
		{Key: "u2", Events: []protocol.TranscriptEvent{ev("b")}, Final: true},
		{Key: "u3", Events: []protocol.TranscriptEvent{ev("c")}, Final: true},
	}
	e := New()
	e.SeekEnd(2)
	got := e.Next(units)
	if len(got) != 1 || got[0].Text != "c" || got[0].Seq != 3 {
		t.Fatalf("resumed = %+v", got)
	}
}
