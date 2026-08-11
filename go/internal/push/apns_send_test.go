package push

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Send tested against a stub standing in for Apple.
//
// This verifies everything muxscale controls: the path, the headers, the
// authorization token, the body, and how each response status is interpreted.
// What it cannot verify is that Apple itself accepts the request -- that needs
// a real .p8, a real bundle id, and a real device token. The gap is exactly one
// thing, and it is worth being precise that the request construction is not
// part of it.

func stubAPNs(t *testing.T, handler http.HandlerFunc) (*APNs, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p8, _ := testP8(t)
	a, err := NewAPNs(APNsConfig{
		KeyID: "KEY123", TeamID: "TEAM456", BundleID: "com.muxscale.app",
		PrivateKeyPEM: p8, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Point the transport at the stub instead of Apple.
	a.host = srv.URL
	return a, srv
}

func TestAPNsSendRequestShape(t *testing.T) {
	var (
		gotPath   string
		gotMethod string
		gotHeader http.Header
		gotBody   []byte
	)
	a, _ := stubAPNs(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotHeader = r.URL.Path, r.Method, r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	n, _ := Build(approvalContext(), DetailMinimal, []string{"dev_1"}, "hk_1")
	n.Sealed = map[string]string{"dev_1": "SEALEDBLOB=="}
	res := a.Send(n, Token{DeviceID: "dev_1", Provider: "apns", Token: "abc123devicetoken"})

	if !res.OK {
		t.Fatalf("send failed: %v", res.Err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s", gotMethod)
	}
	if gotPath != "/3/device/abc123devicetoken" {
		t.Errorf("path = %q; APNs addresses the device in the path", gotPath)
	}

	// Each header has a specific job; a missing one is a silent behaviour change.
	checks := map[string]string{
		"Apns-Topic":     "com.muxscale.app",
		"Apns-Push-Type": "alert",
		"Apns-Priority":  "10",
		"Content-Type":   "application/json",
	}
	for k, want := range checks {
		if got := gotHeader.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if got := gotHeader.Get("Apns-Collapse-Id"); got != "sess_1" {
		t.Errorf("collapse id = %q; a chatty session must replace, not stack", got)
	}

	auth := gotHeader.Get("Authorization")
	if !strings.HasPrefix(auth, "bearer ") {
		t.Errorf("authorization = %q; APNs wants a lowercase bearer prefix", auth)
	}
	if strings.Count(strings.TrimPrefix(auth, "bearer "), ".") != 2 {
		t.Errorf("authorization does not carry a three-part JWT: %q", auth)
	}

	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["muxscale"] != "SEALEDBLOB==" {
		t.Error("the sealed blob for this device was not carried")
	}
	aps := body["aps"].(map[string]any)
	if aps["mutable-content"] != float64(1) {
		t.Error("mutable-content must be set or the extension never runs")
	}
}

// Each device gets its own sealed blob. Sending the wrong one would mean a
// notification that cannot be decrypted, which looks like a silent failure.
func TestAPNsSendPicksThePerDeviceBlob(t *testing.T) {
	var bodies []string
	a, _ := stubAPNs(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.WriteHeader(http.StatusOK)
	})

	n, _ := Build(approvalContext(), DetailMinimal, []string{"dev_a", "dev_b"}, "hk")
	n.Sealed = map[string]string{"dev_a": "FOR_A==", "dev_b": "FOR_B=="}

	a.Send(n, Token{DeviceID: "dev_a", Provider: "apns", Token: "ta"})
	a.Send(n, Token{DeviceID: "dev_b", Provider: "apns", Token: "tb"})

	if !strings.Contains(bodies[0], "FOR_A==") || strings.Contains(bodies[0], "FOR_B==") {
		t.Errorf("device a got the wrong blob: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], "FOR_B==") {
		t.Errorf("device b got the wrong blob: %s", bodies[1])
	}
}

// A dead token must be pruned rather than retried forever.
func TestAPNsSendInterpretsRealResponses(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		ok           bool
		unregistered bool
	}{
		{"accepted", 200, "", true, false},
		{"unregistered", 410, `{"reason":"Unregistered"}`, false, true},
		{"bad token", 400, `{"reason":"BadDeviceToken"}`, false, true},
		{"bad topic", 400, `{"reason":"BadTopic"}`, false, false},
		{"expired provider token", 403, `{"reason":"ExpiredProviderToken"}`, false, false},
		{"rate limited", 429, `{"reason":"TooManyRequests"}`, false, false},
		{"apple outage", 503, `{"reason":"ServiceUnavailable"}`, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, _ := stubAPNs(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			})
			n, _ := Build(approvalContext(), DetailMinimal, []string{"d"}, "hk")
			res := a.Send(n, Token{DeviceID: "d", Provider: "apns", Token: "t"})

			if res.OK != c.ok {
				t.Errorf("ok = %v, want %v", res.OK, c.ok)
			}
			if res.Unregistered != c.unregistered {
				t.Errorf("unregistered = %v, want %v (a dead token retried forever "+
					"is how a push backend accumulates garbage)", res.Unregistered, c.unregistered)
			}
			if !res.OK && res.Err == nil {
				t.Error("a failure must carry an error")
			}
		})
	}
}

// A provider token is signed once and reused, not regenerated per send: Apple
// rejects clients that mint them too often.
func TestAPNsReusesTheProviderTokenAcrossSends(t *testing.T) {
	var auths []string
	a, _ := stubAPNs(t, func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	})
	n, _ := Build(approvalContext(), DetailMinimal, []string{"d"}, "hk")
	for i := 0; i < 5; i++ {
		a.Send(n, Token{DeviceID: "d", Provider: "apns", Token: "t"})
	}
	for i := 1; i < len(auths); i++ {
		if auths[i] != auths[0] {
			t.Fatal("the provider token was regenerated between sends; Apple rate-limits that")
		}
	}
}

// A dispatcher failure must not lose the other devices.
func TestAPNsThroughDispatcherPrunesDeadTokens(t *testing.T) {
	a, _ := stubAPNs(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "deadtoken") {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	var pruned []string
	d := &Dispatcher{
		Transports: map[string]Transport{"apns": a},
		OnDead:     func(tok Token) { pruned = append(pruned, tok.Token) },
	}
	n, _ := Build(approvalContext(), DetailMinimal, []string{"d1", "d2"}, "hk")
	results := d.Dispatch(n, []Token{
		{DeviceID: "d1", Provider: "apns", Token: "deadtoken"},
		{DeviceID: "d2", Provider: "apns", Token: "livetoken"},
	})

	if Delivered(results) != 1 {
		t.Errorf("the live device should still have received it: %+v", results)
	}
	if len(pruned) != 1 || pruned[0] != "deadtoken" {
		t.Errorf("pruned = %v, want just the dead token", pruned)
	}
}

func TestAPNsSendSurfacesNetworkFailure(t *testing.T) {
	p8, _ := testP8(t)
	a, err := NewAPNs(APNsConfig{
		KeyID: "K", TeamID: "T", BundleID: "b", PrivateKeyPEM: p8,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.host = "http://127.0.0.1:1" // nothing listening

	n, _ := Build(approvalContext(), DetailMinimal, []string{"d"}, "hk")
	res := a.Send(n, Token{DeviceID: "d", Provider: "apns", Token: "t"})
	if res.OK {
		t.Error("a send to a dead host reported success")
	}
	if res.Unregistered {
		t.Error("a network failure must not be mistaken for a dead token; " +
			"pruning on an outage would unregister every device")
	}
	if res.Err == nil {
		t.Error("no error reported")
	}
	_ = base64.StdEncoding
}
