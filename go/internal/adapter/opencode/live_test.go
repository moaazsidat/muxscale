package opencode

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// Live check against a real `opencode serve`, skipped when none is running.
func TestAgainstRealServer(t *testing.T) {
	base := os.Getenv("MUXSCALE_OPENCODE_URL")
	if base == "" {
		t.Skip("set MUXSCALE_OPENCODE_URL to run against a live server")
	}
	if _, err := http.Get(base + "/api/health"); err != nil {
		t.Skipf("no server at %s: %v", base, err)
	}
	c := NewClient(base)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Health(ctx); err != nil {
		t.Fatalf("health against the real server: %v", err)
	}
	sessions, err := c.Sessions(ctx)
	if err != nil {
		t.Fatalf("Sessions against the real server: %v", err)
	}
	t.Logf("real server reported %d session(s)", len(sessions))
	for _, s := range sessions {
		t.Logf("  %s title=%q dir=%s model=%s child=%v", s.ID, s.Title, s.Cwd(), s.ModelName(), s.IsChild())
	}

	// The event stream must connect and deliver server.connected.
	sctx, scancel := context.WithTimeout(ctx, 4*time.Second)
	defer scancel()
	got := make(chan string, 4)
	go func() {
		_ = StreamEvents(sctx, base, func(e Event) {
			select {
			case got <- e.Type:
			default:
			}
		})
	}()
	select {
	case typ := <-got:
		t.Logf("first live event: %s", typ)
	case <-sctx.Done():
		t.Error("no event received from the live stream")
	}
}
