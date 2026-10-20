package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/adapter/omp"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

func testOMPServer(t *testing.T) (*httptest.Server, *Registry) {
	t.Helper()
	reg := NewRegistry()
	adapters := adapter.NewRegistry()
	adapters.MustRegister(omp.New())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	NewHookServer(reg, adapters, "secret-token", time.Second, log).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, reg
}

// omp's extension reports lifecycle events (approvals: keyapprove_test.go). The daemon must
// turn turn_end into WAITING_INPUT and ignore a non-terminal agent_end, which
// is maintenance work rather than a finished turn.
func TestOMPLifecycleHooksDriveState(t *testing.T) {
	srv, reg := testOMPServer(t)

	post := func(body map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/hook/omp", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer secret-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	base := map[string]any{"v": 1, "agent": "omp", "session_id": "s1", "cwd": "/w", "session_file": ""}
	base["event"] = "session_start"
	if out := post(base); out["decision"] != "none" {
		t.Fatalf("reply = %+v", out)
	}
	id, ok := reg.IDFor("omp", "s1")
	if !ok {
		t.Fatal("session not registered")
	}
	if s, _ := reg.Get(id); s.State != protocol.StateRunning || s.Guess.Confidence != 1 {
		t.Fatalf("after start = %+v", s)
	}

	base["event"] = "turn_end"
	post(base)
	if s, _ := reg.Get(id); s.State != protocol.StateWaitingInput {
		t.Fatalf("after turn_end = %+v", s)
	}

	// A non-terminal agent_end means work will resume; it must not look like
	// a finished turn to the person holding the phone.
	base["event"] = "agent_end"
	base["is_terminal"] = false
	post(base)
	if s, _ := reg.Get(id); s.State != protocol.StateWaitingInput {
		t.Fatalf("non-terminal agent_end changed state: %+v", s)
	}

	base["event"] = "session_shutdown"
	post(base)
	if s, _ := reg.Get(id); s.State != protocol.StateExited {
		t.Fatalf("after shutdown = %+v", s)
	}
}
