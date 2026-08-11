package push

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A generated service-account key, so the whole FCM path is testable without a
// Firebase project.
func testServiceAccount(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	acct := map[string]string{
		"type":         "service_account",
		"project_id":   "muxscale-test",
		"private_key":  string(pemKey),
		"client_email": "push@muxscale-test.iam.gserviceaccount.com",
		"token_uri":    "https://oauth2.googleapis.com/token",
	}
	b, _ := json.Marshal(acct)
	return b
}

// stubFCM serves both the token exchange and the send endpoint.
func stubFCM(t *testing.T, onSend http.HandlerFunc) (*FCM, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("assertion") == "" {
			t.Error("no assertion sent")
		}
		_, _ = w.Write([]byte(`{"access_token":"ya29.test-token","expires_in":3599}`))
	})
	mux.HandleFunc("/send", onSend)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	f, err := NewFCM(FCMConfig{
		ServiceAccountJSON: testServiceAccount(t),
		HTTPClient:         srv.Client(),
		TokenURL:           srv.URL + "/token",
		SendURL:            srv.URL + "/send",
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, srv
}

func TestFCMRejectsABadServiceAccount(t *testing.T) {
	for name, key := range map[string][]byte{
		"not json":       []byte("nope"),
		"missing fields": []byte(`{"type":"service_account"}`),
		"bad pem":        []byte(`{"project_id":"p","client_email":"e","private_key":"not pem"}`),
	} {
		if _, err := NewFCM(FCMConfig{ServiceAccountJSON: key}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// The message should name the file so an operator knows what to fix.
	_, err := NewFCM(FCMConfig{ServiceAccountJSON: []byte("nope")})
	if !strings.Contains(err.Error(), "Firebase") {
		t.Errorf("the error should point at where the key comes from: %v", err)
	}
}

// Data-only is the whole point on Android: a notification payload is rendered
// by the system before the app runs, so the sealed content would never be
// decrypted and the lock screen would only ever show the minimal cloud text.
func TestFCMPayloadIsDataOnly(t *testing.T) {
	n, _ := Build(approvalContext(), DetailFull, []string{"d1"}, "hk")
	body, err := BuildFCMPayload(n, "devtoken", "SEALED==")
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatal(err)
	}
	inner := msg["message"].(map[string]any)

	if _, has := inner["notification"]; has {
		t.Error("a notification payload would be rendered before the app runs, " +
			"so the sealed content would never be decrypted")
	}
	data := inner["data"].(map[string]any)
	if data["muxscale"] != "SEALED==" {
		t.Error("the sealed blob was not carried")
	}
	if data["requires_unlock"] != "1" {
		t.Error("an approval must be marked as requiring unlock")
	}
	android := inner["android"].(map[string]any)
	if android["priority"] != "high" {
		t.Errorf("priority = %v; the product is 'tell me now'", android["priority"])
	}
	if android["collapse_key"] != "sess_1" {
		t.Errorf("collapse_key = %v", android["collapse_key"])
	}
}

// Same canary rule as everywhere else: no content in what crosses Google.
func TestFCMPayloadCarriesNoPlaintextContent(t *testing.T) {
	n, _ := Build(approvalContext(), DetailFull, []string{"d1"}, "hk")
	body, _ := BuildFCMPayload(n, "tok", "OPAQUE==")
	wire := string(body)
	for _, c := range []string{"rm -rf", "billing adapter", "sk-live", "secret-project"} {
		if strings.Contains(wire, c) {
			t.Errorf("the FCM payload leaked %q: %s", c, wire)
		}
	}
}

func TestFCMSendRequestShape(t *testing.T) {
	var gotAuth string
	var gotBody []byte
	f, _ := stubFCM(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	n, _ := Build(approvalContext(), DetailMinimal, []string{"d1"}, "hk")
	n.Sealed = map[string]string{"d1": "BLOB=="}
	res := f.Send(n, Token{DeviceID: "d1", Provider: "fcm", Token: "androidtoken"})

	if !res.OK {
		t.Fatalf("send failed: %v", res.Err)
	}
	if gotAuth != "Bearer ya29.test-token" {
		t.Errorf("authorization = %q; the OAuth token was not used", gotAuth)
	}
	var msg map[string]any
	_ = json.Unmarshal(gotBody, &msg)
	inner := msg["message"].(map[string]any)
	if inner["token"] != "androidtoken" {
		t.Errorf("token = %v", inner["token"])
	}
}

// The RS256 assertion must verify against its own key, not merely be shaped
// right.
func TestFCMAssertionVerifies(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tok, err := signRS256JWT(key, "svc@example.com", fcmScope, "https://oauth2.googleapis.com/token", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	var header map[string]string
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	_ = json.Unmarshal(hb, &header)
	if header["alg"] != "RS256" {
		t.Errorf("alg = %q, want RS256", header["alg"])
	}

	var claims map[string]any
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(cb, &claims)
	if claims["scope"] != fcmScope {
		t.Errorf("scope = %v", claims["scope"])
	}
	if claims["iss"] != "svc@example.com" {
		t.Errorf("iss = %v", claims["iss"])
	}
	if _, ok := claims["exp"]; !ok {
		t.Error("exp is required or Google rejects the assertion")
	}

	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256Sum([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, cryptoSHA256(), digest[:], sig); err != nil {
		t.Errorf("the assertion does not verify against its own key: %v", err)
	}
}

func TestFCMCachesTheAccessToken(t *testing.T) {
	var exchanges int
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		exchanges++
		_, _ = fmt.Fprintf(w, `{"access_token":"tok-%d","expires_in":3599}`, exchanges)
	})
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f, err := NewFCM(FCMConfig{
		ServiceAccountJSON: testServiceAccount(t), HTTPClient: srv.Client(),
		TokenURL: srv.URL + "/token", SendURL: srv.URL + "/send",
	})
	if err != nil {
		t.Fatal(err)
	}
	n, _ := Build(approvalContext(), DetailMinimal, []string{"d"}, "hk")
	for i := 0; i < 5; i++ {
		f.Send(n, Token{DeviceID: "d", Provider: "fcm", Token: "t"})
	}
	if exchanges != 1 {
		t.Errorf("exchanged the key for a token %d times; it should be cached", exchanges)
	}
}

// A dead token must be pruned. UNREGISTERED is FCM's word for APNs's 410.
func TestFCMStatusInterpretation(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		ok           bool
		unregistered bool
	}{
		{"accepted", 200, `{"name":"projects/p/messages/1"}`, true, false},
		{"unregistered", 404, `{"error":{"status":"NOT_FOUND"}}`, false, true},
		{"unregistered by detail", 400, `{"error":{"details":[{"errorCode":"UNREGISTERED"}]}}`, false, true},
		{"bad token argument", 400, `{"error":{"status":"INVALID_ARGUMENT","message":"invalid token"}}`, false, true},
		{"rate limited", 429, `{}`, false, false},
		{"google outage", 503, `{}`, false, false},
		{"misconfigured", 403, `{"error":{"status":"PERMISSION_DENIED"}}`, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := interpretFCMStatus(c.status, c.body)
			if got.OK != c.ok {
				t.Errorf("ok = %v, want %v", got.OK, c.ok)
			}
			if got.Unregistered != c.unregistered {
				t.Errorf("unregistered = %v, want %v", got.Unregistered, c.unregistered)
			}
			if !got.OK && got.Err == nil {
				t.Error("a failure needs an error")
			}
		})
	}
}

// Both providers side by side, which is the real deployment.
func TestBothProvidersThroughOneDispatcher(t *testing.T) {
	apns, _ := stubAPNs(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	fcm, _ := stubFCM(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	d := &Dispatcher{Transports: map[string]Transport{"apns": apns, "fcm": fcm}}
	n, _ := Build(approvalContext(), DetailMinimal, []string{"iphone", "pixel"}, "hk")
	n.Sealed = map[string]string{"iphone": "A==", "pixel": "B=="}

	results := d.Dispatch(n, []Token{
		{DeviceID: "iphone", Provider: "apns", Token: "ios-token"},
		{DeviceID: "pixel", Provider: "fcm", Token: "android-token"},
	})
	if Delivered(results) != 2 {
		t.Errorf("delivered %d of 2: %+v", Delivered(results), results)
	}
}
