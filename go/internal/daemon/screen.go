package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/muxscale/muxscale/go/internal/vt"
)

// The endpoints a remote client needs to render and drive a terminal, as
// opposed to reading a transcript. The chat surface never touches any of this;
// it exists for the shell a phone opens in an agent's working directory.

const (
	// defaultFPS is spec §5.4's default. Frames are produced by diffing on a
	// tick, so a burst of output collapses into one frame rather than queueing:
	// the client gets the current screen, never a backlog to fast-forward.
	defaultFPS = 12
	minFPS     = 4
	maxFPS     = 20

	// keyframeEvery re-sends a full snapshot periodically. A client that
	// dropped a delta -- backgrounded, or on a flaky cellular link -- would
	// otherwise render a subtly wrong screen forever with no way to notice.
	keyframeEvery = 10 * time.Second
)

func (d *Daemon) screenRoutes(mux *http.ServeMux) {
	// POST /v1/sessions/shell {"session_id":"...","cols":N,"rows":N}
	//
	// Opens a login shell in the named session's working directory. The cwd is
	// resolved from the session, never accepted from the caller.
	mux.HandleFunc("/v1/sessions/shell", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SessionID string `json:"session_id"`
			Cols      uint16 `json:"cols"`
			Rows      uint16 `json:"rows"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SessionID == "" {
			http.Error(w, "bad request: session_id required", http.StatusBadRequest)
			return
		}
		if body.Cols == 0 {
			body.Cols = 80
		}
		if body.Rows == 0 {
			body.Rows = 24
		}
		sess, err := d.mgr.RunShell(body.SessionID, body.Cols, body.Rows)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, sess)
	})

	// POST /v1/sessions/resize {"session_id":"...","cols":N,"rows":N}
	mux.HandleFunc("/v1/sessions/resize", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SessionID string `json:"session_id"`
			Cols      uint16 `json:"cols"`
			Rows      uint16 `json:"rows"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := d.mgr.Resize(body.SessionID, body.Cols, body.Rows); err != nil {
			// A refusal here is a policy decision, not a fault: the client asked
			// for something the session's other viewers forbid.
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, map[string]bool{"resized": true})
	})

	// GET /v1/sessions/screen/stream?session=ID[&fps=N]
	//
	// SSE. One `snapshot` event, then `delta` events for the rows that changed,
	// with a periodic snapshot as a resync point.
	mux.HandleFunc("/v1/sessions/screen/stream", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("session")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		shadow, ok := d.mgr.ScreenClone(id)
		if !ok {
			http.Error(w, "session has no screen (only wrapped sessions do)", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		enc := json.NewEncoder(w)
		send := func(event string, v any) bool {
			if _, err := fmt.Fprintf(w, "event: %s\ndata: ", event); err != nil {
				return false
			}
			if err := enc.Encode(v); err != nil {
				return false
			}
			if _, err := fmt.Fprint(w, "\n"); err != nil {
				return false
			}
			flusher.Flush()
			return true
		}

		var seq uint64
		snapshot := func(g *vt.Grid) bool {
			snap := g.Snapshot()
			return send("snapshot", struct {
				vt.Snapshot
				Seq uint64 `json:"seq"`
			}{snap, seq})
		}
		if !snapshot(shadow) {
			return
		}

		ticker := time.NewTicker(time.Second / time.Duration(clampFPS(r.URL.Query().Get("fps"))))
		defer ticker.Stop()
		keyframe := time.NewTicker(keyframeEvery)
		defer keyframe.Stop()

		for {
			select {
			case <-r.Context().Done():
				return

			case <-keyframe.C:
				cur, ok := d.mgr.ScreenClone(id)
				if !ok {
					return
				}
				seq++
				shadow = cur
				if !snapshot(shadow) {
					return
				}

			case <-ticker.C:
				cur, ok := d.mgr.ScreenClone(id)
				if !ok {
					// The session ended. Say so rather than leaving the client
					// waiting on a stream that will never produce another frame.
					send("closed", map[string]string{"session": id})
					return
				}
				lines := cur.Diff(shadow)
				if len(lines) == 0 {
					continue
				}
				base := seq
				seq++
				snap := cur.Snapshot()
				if !send("delta", vt.Delta{
					BaseSeq: base, Seq: seq,
					Cols: snap.Cols, Rows: snap.Rows,
					CursorX: snap.CursorX, CursorY: snap.CursorY,
					CursorOff: snap.CursorOff,
					Lines:     lines,
				}) {
					return
				}
				shadow = cur
			}
		}
	})
}

// clampFPS reads the client's frame-rate hint. Out-of-range and unparseable
// both fall back to the default: a client asking for 1000 fps is asking for the
// daemon to spend a core on diffing, and the answer is no rather than an error
// that breaks an otherwise working stream.
func clampFPS(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultFPS
	}
	if n < minFPS {
		return minFPS
	}
	if n > maxFPS {
		return maxFPS
	}
	return n
}
