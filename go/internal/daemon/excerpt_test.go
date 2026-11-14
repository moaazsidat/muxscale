package daemon

import (
	"strings"
	"testing"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// A long message is stored whole and sent cut short, marked so a phone can
// offer See all; tool output is cut when stored, since nothing asks for more.
func TestLongMessagesAreKeptWholeAndSentShort(t *testing.T) {
	long := strings.Repeat("é", excerptBytes) // 2 bytes a rune
	msg := protocol.TranscriptEvent{Kind: protocol.EventAssistantMessage, Text: long}

	if k := kept(msg); k.Text != long || k.Truncated {
		t.Fatalf("stored %d bytes, truncated %v", len(k.Text), k.Truncated)
	}
	s := sent(msg)
	if len(s.Text) > excerptBytes || !s.Truncated || !strings.HasPrefix(long, s.Text) {
		t.Fatalf("sent %d bytes, truncated %v", len(s.Text), s.Truncated)
	}

	out := kept(protocol.TranscriptEvent{Kind: protocol.EventToolResult, ToolOutput: long})
	if len(out.ToolOutput) > excerptBytes || !out.Truncated {
		t.Fatalf("tool output stored at %d bytes", len(out.ToolOutput))
	}

	short := sent(protocol.TranscriptEvent{Kind: protocol.EventUserMessage, Text: "hi"})
	if short.Text != "hi" || short.Truncated {
		t.Fatalf("short message sent as %q, truncated %v", short.Text, short.Truncated)
	}
}

func TestStoreHandsBackOneEventWhole(t *testing.T) {
	_, _, st := testManager(t)
	long := strings.Repeat("x", 3*excerptBytes)
	if err := st.AppendEvents([]protocol.TranscriptEvent{{
		SessionID: "s", Seq: 7, ID: "e", Kind: protocol.EventAssistantMessage, Text: long,
	}}); err != nil {
		t.Fatal(err)
	}
	ev, ok, err := st.Event("s", 7)
	if err != nil || !ok || ev.Text != long {
		t.Fatalf("got %d bytes, found %v, %v", len(ev.Text), ok, err)
	}
	if _, ok, _ := st.Event("s", 8); ok {
		t.Fatal("found an event that isn't there")
	}
}
