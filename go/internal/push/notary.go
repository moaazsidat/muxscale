package push

// The daemon's side of the push notary: hand it a sealed notification and the
// capability the phone gave us. The notary holds the APNs key and the phone's
// device token; the daemon holds neither.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultNotary is muxscale's hosted notary. MUXSCALE_PUSH_URL overrides it.
const DefaultNotary = "https://push.muxscale.com"

// ErrGone means the phone's registration no longer exists: it turned
// notifications off, registered again, or Apple retired its token. The
// device should be forgotten until the phone sends a new capability.
var ErrGone = errors.New("the phone's push registration is gone")

// Notary sends to the hosted notary.
type Notary struct {
	URL    string
	Client *http.Client
}

// NewNotary returns a client for url, or the default when url is empty.
func NewNotary(url string) *Notary {
	if url == "" {
		url = DefaultNotary
	}
	return &Notary{URL: strings.TrimRight(url, "/"), Client: &http.Client{Timeout: 10 * time.Second}}
}

// Push is one notification for one phone.
type Push struct {
	Capability string
	Kind       string
	CollapseID string
	ThreadID   string
	Sealed     string
}

// Send delivers a push. ErrGone means drop the device; any other error is
// worth logging and not worth retrying: by the next one, things have moved on.
func (n *Notary) Send(p Push) error {
	body, err := json.Marshal(map[string]string{
		"kind": p.Kind, "collapse_id": p.CollapseID, "thread_id": p.ThreadID, "sealed": p.Sealed,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, n.URL+"/v1/push/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Capability "+p.Capability)
	req.Header.Set("Content-Type", "application/json")
	res, err := n.Client.Do(req)
	if err != nil {
		return fmt.Errorf("reaching the push notary: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		return nil
	}
	if res.StatusCode == http.StatusGone {
		return ErrGone
	}
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return fmt.Errorf("push notary: %d %s: %s", res.StatusCode, e.Error, e.Message)
	}
	return fmt.Errorf("push notary: %d", res.StatusCode)
}
