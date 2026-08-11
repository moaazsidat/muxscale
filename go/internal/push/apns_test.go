package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A generated P-256 key in .p8 form, so everything up to the network call is
// testable without an Apple Developer account.
func testP8(t *testing.T) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), key
}

func TestParseP8(t *testing.T) {
	p8, want := testP8(t)
	got, err := ParseP8(p8)
	if err != nil {
		t.Fatal(err)
	}
	if got.D.Cmp(want.D) != 0 {
		t.Error("parsed key does not match")
	}
}

// The error must say what the operator got wrong. "asn1: structure error" sends
// people down the wrong path; "is this the .p8 file?" does not.
func TestParseP8ErrorsAreActionable(t *testing.T) {
	if _, err := ParseP8(nil); err == nil {
		t.Error("empty key should error")
	}
	_, err := ParseP8([]byte("definitely not pem"))
	if err == nil {
		t.Fatal("garbage should error")
	}
	if !strings.Contains(err.Error(), ".p8") {
		t.Errorf("the error should point at the .p8 file: %v", err)
	}
}

// JWS wants fixed-width r||s, not an ASN.1 sequence. Getting this wrong yields
// a token Apple rejects with an opaque error, so it is asserted structurally.
func TestES256JWTStructure(t *testing.T) {
	_, key := testP8(t)
	tok, err := signES256JWT(key, "KEY123", "TEAM456", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}

	var header map[string]string
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("header is not raw-url base64: %v", err)
	}
	if err := json.Unmarshal(hb, &header); err != nil {
		t.Fatal(err)
	}
	if header["alg"] != "ES256" || header["kid"] != "KEY123" || header["typ"] != "JWT" {
		t.Errorf("header = %v", header)
	}

	var claims map[string]any
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if err := json.Unmarshal(cb, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["iss"] != "TEAM456" {
		t.Errorf("iss = %v", claims["iss"])
	}
	if _, ok := claims["iat"]; !ok {
		t.Error("iat is required")
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("signature is not raw-url base64: %v", err)
	}
	if len(sig) != 64 {
		t.Errorf("signature is %d bytes, want exactly 64 (fixed-width r||s, not ASN.1)", len(sig))
	}
}

// The signature must actually verify against the key, not merely be shaped right.
func TestES256JWTVerifies(t *testing.T) {
	_, key := testP8(t)
	tok, err := signES256JWT(key, "K", "T", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	signing := parts[0] + "." + parts[1]
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])

	digest := sha256Sum([]byte(signing))
	var r, s = new(bigInt), new(bigInt)
	r.SetBytes(sig[:32])
	s.SetBytes(sig[32:])
	if !ecdsa.Verify(&key.PublicKey, digest[:], r, s) {
		t.Error("the signature does not verify against its own key")
	}
}

// Apple rejects tokens older than an hour, and also rejects regenerating them
// too often. Caching sits between the two.
func TestProviderTokenIsCachedThenRefreshed(t *testing.T) {
	p8, _ := testP8(t)
	a, err := NewAPNs(APNsConfig{KeyID: "K", TeamID: "T", BundleID: "com.muxscale.app", PrivateKeyPEM: p8})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	first, err := a.providerToken(now)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := a.providerToken(now.Add(time.Minute))
	if first != again {
		t.Error("the token should be reused within its lifetime")
	}
	later, _ := a.providerToken(now.Add(apnsTokenLifetime + time.Minute))
	if later == first {
		t.Error("the token should be refreshed once it ages out")
	}
}

func TestNewAPNsRequiresConfig(t *testing.T) {
	p8, _ := testP8(t)
	for name, cfg := range map[string]APNsConfig{
		"no key id":    {TeamID: "T", BundleID: "b", PrivateKeyPEM: p8},
		"no team id":   {KeyID: "K", BundleID: "b", PrivateKeyPEM: p8},
		"no bundle id": {KeyID: "K", TeamID: "T", PrivateKeyPEM: p8},
		"no key":       {KeyID: "K", TeamID: "T", BundleID: "b"},
	} {
		if _, err := NewAPNs(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// Without mutable-content the extension never runs, and the user only ever
// sees the minimal cloud text however much detail they chose.
func TestAPNsPayloadEnablesTheExtension(t *testing.T) {
	n, _ := Build(approvalContext(), DetailFull, []string{"d1"}, "hk")
	body, err := BuildAPNsPayload(n, "SEALEDBLOB==")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	aps := got["aps"].(map[string]any)
	if aps["mutable-content"] != float64(1) {
		t.Error("mutable-content must be 1 or the notification extension never runs")
	}
	if aps["interruption-level"] != "time-sensitive" {
		t.Errorf("an approval should be time-sensitive, got %v", aps["interruption-level"])
	}
	if aps["category"] != "MUXSCALE_APPROVAL" {
		t.Errorf("category = %v", aps["category"])
	}
	if got["muxscale"] != "SEALEDBLOB==" {
		t.Error("the sealed blob must be carried for on-device decryption")
	}
}

// The payload crossing Apple must not carry content either.
func TestAPNsPayloadCarriesNoPlaintextContent(t *testing.T) {
	n, _ := Build(approvalContext(), DetailFull, []string{"d1"}, "hk")
	body, _ := BuildAPNsPayload(n, "OPAQUE==")
	wire := string(body)
	for _, c := range []string{"rm -rf", "billing adapter", "sk-live", "secret-project"} {
		if strings.Contains(wire, c) {
			t.Errorf("the APNs payload leaked %q: %s", c, wire)
		}
	}
}

// A dead token retried forever is how a push backend accumulates permanent
// garbage.
func TestAPNsStatusInterpretation(t *testing.T) {
	cases := []struct {
		status       int
		body         string
		ok           bool
		unregistered bool
	}{
		{http.StatusOK, "", true, false},
		{http.StatusGone, `{"reason":"Unregistered"}`, false, true},
		{http.StatusBadRequest, `{"reason":"BadDeviceToken"}`, false, true},
		{http.StatusBadRequest, `{"reason":"BadTopic"}`, false, false},
		{http.StatusTooManyRequests, `{"reason":"TooManyRequests"}`, false, false},
		{http.StatusInternalServerError, "", false, false},
		{http.StatusForbidden, `{"reason":"ExpiredProviderToken"}`, false, false},
	}
	for _, c := range cases {
		got := interpretAPNsStatus(c.status, c.body)
		if got.OK != c.ok {
			t.Errorf("status %d: ok = %v, want %v", c.status, got.OK, c.ok)
		}
		if got.Unregistered != c.unregistered {
			t.Errorf("status %d: unregistered = %v, want %v", c.status, got.Unregistered, c.unregistered)
		}
		if !got.OK && got.Err == nil {
			t.Errorf("status %d: a failure needs an error", c.status)
		}
	}
}

func TestCollapseIDIsBounded(t *testing.T) {
	long := strings.Repeat("x", 200)
	if got := truncateForHeader(long, 64); len(got) != 64 {
		t.Errorf("collapse id is %d chars; APNs caps the header at 64", len(got))
	}
	if got := truncateForHeader("short", 64); got != "short" {
		t.Errorf("a short value should pass through, got %q", got)
	}
}
