package daemon

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A quiet event stream must still carry bytes, or a client cannot tell an idle
// daemon from a dead connection.
func TestEventsStreamSendsKeepalives(t *testing.T) {
	old := eventsKeepalive
	eventsKeepalive = 20 * time.Millisecond
	t.Cleanup(func() { eventsKeepalive = old })

	_, mux := authDaemon(t)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), ":") {
			return
		}
	}
	t.Fatalf("no keepalive comment before the stream ended: %v", sc.Err())
}
