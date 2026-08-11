package e2e

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// pair runs a full handshake and returns both sides' sessions.
func pair(t *testing.T) (daemonKP, deviceKP KeyPair, dSess, pSess *Session, dHS, pHS *Handshake) {
	t.Helper()
	var err error
	daemonKP, err = GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	deviceKP, err = GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}

	// The device knows the daemon's key from the QR code.
	pHS, err = NewHandshake(RoleDevice, deviceKP, daemonKP.Public)
	if err != nil {
		t.Fatal(err)
	}
	dHS, err = NewHandshake(RoleDaemon, daemonKP, nil)
	if err != nil {
		t.Fatal(err)
	}

	msg1, err := pHS.WriteMessage(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dHS.ReadMessage(msg1); err != nil {
		t.Fatal(err)
	}
	msg2, err := dHS.WriteMessage(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pHS.ReadMessage(msg2); err != nil {
		t.Fatal(err)
	}

	if !pHS.Complete() || !dHS.Complete() {
		t.Fatal("handshake did not complete")
	}
	dSess, err = dHS.Session()
	if err != nil {
		t.Fatal(err)
	}
	pSess, err = pHS.Session()
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestHandshakeAndRoundTrip(t *testing.T) {
	_, deviceKP, dSess, pSess, dHS, _ := pair(t)

	// The daemon learns the device's key during the handshake, which is what
	// it records when enrolling the device.
	if !bytes.Equal(dHS.PeerStatic(), deviceKP.Public) {
		t.Error("the daemon did not learn the device's public key")
	}

	hdr := RoutingHeader{Version: 1, HostKey: "hk_1", DeviceID: "dev_1", StreamID: 7}
	env, err := Seal(pSess, hdr, []byte("approve the rm -rf"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(dSess, env)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "approve the rm -rf" {
		t.Errorf("round trip = %q", got)
	}
}

// The relay is an untrusted forwarder. This is the test that says so: nothing
// recognisable from the plaintext may appear anywhere in what crosses it.
func TestRelaySeesNoPlaintext(t *testing.T) {
	_, _, _, pSess, _, _ := pair(t)

	// AWS's own documentation example key, used as a canary so the assertion
	// below has something recognisable to look for.
	secret := "the deploy key is AKIAIOSFODNN7EXAMPLE" // notasecret
	hdr := RoutingHeader{Version: 1, HostKey: "hk_1", DeviceID: "dev_1", StreamID: 1}
	env, err := Seal(pSess, hdr, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}

	// Everything a relay can observe: the header and the ciphertext.
	observable := string(env.Header.Encode()) + env.Ciphertext
	for _, canary := range []string{"deploy key", "AKIAIOSFODNN7EXAMPLE", "the deploy"} { // notasecret
		if strings.Contains(observable, canary) {
			t.Errorf("plaintext %q is visible to the relay", canary)
		}
	}
	raw, _ := base64.StdEncoding.DecodeString(env.Ciphertext)
	if bytes.Contains(raw, []byte("AKIA")) {
		t.Error("plaintext leaked into the ciphertext bytes")
	}
}

// The routing header is authenticated data. A relay that redirects a message to
// a different device must be detected, not silently obeyed.
func TestTamperedRoutingHeaderFailsToOpen(t *testing.T) {
	_, _, dSess, pSess, _, _ := pair(t)

	hdr := RoutingHeader{Version: 1, HostKey: "hk_1", DeviceID: "dev_1", StreamID: 1}
	env, err := Seal(pSess, hdr, []byte("allow"))
	if err != nil {
		t.Fatal(err)
	}

	// A malicious relay rewrites the destination.
	env.Header.DeviceID = "dev_attacker"
	if _, err := Open(dSess, env); err == nil {
		t.Fatal("a redirected message decrypted successfully; the header must be authenticated")
	}
}

func TestTamperedCiphertextFailsToOpen(t *testing.T) {
	_, _, dSess, pSess, _, _ := pair(t)
	hdr := RoutingHeader{Version: 1, HostKey: "h", DeviceID: "d", StreamID: 1}
	env, _ := Seal(pSess, hdr, []byte("deny"))

	raw, _ := base64.StdEncoding.DecodeString(env.Ciphertext)
	raw[len(raw)/2] ^= 0xff
	env.Ciphertext = base64.StdEncoding.EncodeToString(raw)

	if _, err := Open(dSess, env); err == nil {
		t.Fatal("tampered ciphertext decrypted successfully")
	}
}

// A replayed message could be a replayed approval. Noise's nonce sequence must
// reject it.
func TestReplayIsRejected(t *testing.T) {
	_, _, dSess, pSess, _, _ := pair(t)
	hdr := RoutingHeader{Version: 1, HostKey: "h", DeviceID: "d", StreamID: 1}

	env, _ := Seal(pSess, hdr, []byte("allow"))
	if _, err := Open(dSess, env); err != nil {
		t.Fatalf("first delivery should succeed: %v", err)
	}
	if _, err := Open(dSess, env); err == nil {
		t.Fatal("a replayed message was accepted; a replayed approval is a real hazard")
	}
}

// Out-of-order delivery must fail closed rather than being silently accepted.
func TestOutOfOrderIsRejected(t *testing.T) {
	_, _, dSess, pSess, _, _ := pair(t)
	hdr := RoutingHeader{Version: 1, HostKey: "h", DeviceID: "d", StreamID: 1}

	first, _ := Seal(pSess, hdr, []byte("one"))
	second, _ := Seal(pSess, hdr, []byte("two"))

	// Deliver the second message first.
	if _, err := Open(dSess, second); err == nil {
		t.Fatal("out-of-order delivery was accepted")
	}
	_ = first
}

// A device that does not know the daemon's real key cannot complete IK. This is
// what makes the QR code the root of trust.
func TestWrongDaemonKeyFailsHandshake(t *testing.T) {
	daemonKP, _ := GenerateKeyPair()
	deviceKP, _ := GenerateKeyPair()
	imposterKP, _ := GenerateKeyPair()

	// The device was given the wrong daemon key.
	pHS, err := NewHandshake(RoleDevice, deviceKP, imposterKP.Public)
	if err != nil {
		t.Fatal(err)
	}
	dHS, err := NewHandshake(RoleDaemon, daemonKP, nil)
	if err != nil {
		t.Fatal(err)
	}

	msg1, err := pHS.WriteMessage(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dHS.ReadMessage(msg1); err == nil {
		t.Fatal("the daemon accepted a handshake addressed to a different key")
	}
}

func TestDeviceMustKnowDaemonKey(t *testing.T) {
	kp, _ := GenerateKeyPair()
	if _, err := NewHandshake(RoleDevice, kp, nil); err == nil {
		t.Error("a device with no daemon key should be refused, not left to fail later")
	}
	if _, err := NewHandshake(RoleDevice, kp, []byte("too short")); err == nil {
		t.Error("a malformed daemon key should be refused")
	}
}

// ---- SAS ----

// A relay in the middle necessarily runs two different handshakes, so it cannot
// make both sides display the same words. This is the whole point of the SAS.
func TestSASMatchesForAGenuinePairing(t *testing.T) {
	_, _, _, _, dHS, pHS := pair(t)

	dWords, err := SAS(dHS.ChannelBinding())
	if err != nil {
		t.Fatal(err)
	}
	pWords, err := SAS(pHS.ChannelBinding())
	if err != nil {
		t.Fatal(err)
	}
	if !SASMatches(dWords, pWords) {
		t.Fatalf("both sides of a genuine pairing must show the same words:\n  daemon: %s\n  device: %s",
			SASString(dWords), SASString(pWords))
	}
	if len(dWords) != SASWordCount {
		t.Errorf("got %d words, want %d", len(dWords), SASWordCount)
	}
	t.Logf("SAS: %s", SASString(dWords))
}

// The adversarial case: a relay that terminates one handshake and originates
// another. Both halves succeed cryptographically -- and the words differ, which
// is what the human sees.
func TestSASDiffersUnderARelayInTheMiddle(t *testing.T) {
	daemonKP, _ := GenerateKeyPair()
	deviceKP, _ := GenerateKeyPair()
	relayKP, _ := GenerateKeyPair()

	// Leg 1: the device believes it is talking to the daemon, but the relay
	// substituted its own key.
	deviceHS, _ := NewHandshake(RoleDevice, deviceKP, relayKP.Public)
	relayAsDaemon, _ := NewHandshake(RoleDaemon, relayKP, nil)
	m1, _ := deviceHS.WriteMessage(nil)
	if _, err := relayAsDaemon.ReadMessage(m1); err != nil {
		t.Fatal(err)
	}
	m2, _ := relayAsDaemon.WriteMessage(nil)
	if _, err := deviceHS.ReadMessage(m2); err != nil {
		t.Fatal(err)
	}

	// Leg 2: the relay opens its own handshake to the real daemon.
	relayAsDevice, _ := NewHandshake(RoleDevice, relayKP, daemonKP.Public)
	daemonHS, _ := NewHandshake(RoleDaemon, daemonKP, nil)
	m3, _ := relayAsDevice.WriteMessage(nil)
	if _, err := daemonHS.ReadMessage(m3); err != nil {
		t.Fatal(err)
	}
	m4, _ := relayAsDevice.WriteMessage, daemonHS.WriteMessage
	_ = m4
	m4b, _ := daemonHS.WriteMessage(nil)
	if _, err := relayAsDevice.ReadMessage(m4b); err != nil {
		t.Fatal(err)
	}

	deviceWords, err := SAS(deviceHS.ChannelBinding())
	if err != nil {
		t.Fatal(err)
	}
	daemonWords, err := SAS(daemonHS.ChannelBinding())
	if err != nil {
		t.Fatal(err)
	}

	if SASMatches(deviceWords, daemonWords) {
		t.Fatal("a relay in the middle produced matching words; the SAS check is useless")
	}
	t.Logf("relay-in-the-middle detected:\n  device sees: %s\n  daemon sees: %s",
		SASString(deviceWords), SASString(daemonWords))
}

func TestSASIsDeterministic(t *testing.T) {
	binding := []byte("a fixed channel binding value")
	a, _ := SAS(binding)
	b, _ := SAS(binding)
	if !SASMatches(a, b) {
		t.Error("SAS must be deterministic for the same binding")
	}
	c, _ := SAS([]byte("a different binding"))
	if SASMatches(a, c) {
		t.Error("different bindings must produce different words")
	}
}

func TestSASRequiresABinding(t *testing.T) {
	if _, err := SAS(nil); err == nil {
		t.Error("an empty binding should be an error, not a fixed word list")
	}
}

// A non-power-of-two list would bias early words and quietly reduce the
// entropy the check depends on. init() enforces this; the test documents why.
func TestSASWordListIsExactlyAPowerOfTwo(t *testing.T) {
	if len(sasWords) != 256 {
		t.Fatalf("word list has %d entries, want exactly 256", len(sasWords))
	}
	seen := map[string]bool{}
	for _, w := range sasWords {
		if seen[w] {
			t.Errorf("duplicate word %q reduces the effective entropy", w)
		}
		seen[w] = true
		if len(w) < 3 || len(w) > 5 {
			t.Errorf("word %q is not 3-5 characters; the list should stay easy to read aloud", w)
		}
	}
}

// ---- OTP and invites ----

func TestOTPIsSingleUseAndExpires(t *testing.T) {
	otp, err := NewOTP()
	if err != nil {
		t.Fatal(err)
	}
	if otp.Expired() {
		t.Error("a fresh token should not be expired")
	}
	if !otp.Equal(otp.Token) {
		t.Error("a token should match itself")
	}
	if otp.Equal("wrong") {
		t.Error("a wrong token matched")
	}
	if len(otp.ShortCode) != 8 {
		t.Errorf("short code = %q, want 8 characters", otp.ShortCode)
	}

	stale := OTP{Token: "x", ExpiresAt: time.Now().Add(-time.Second)}
	if !stale.Expired() {
		t.Error("a past-TTL token should be expired")
	}
}

func TestOTPsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		otp, err := NewOTP()
		if err != nil {
			t.Fatal(err)
		}
		if seen[otp.Token] {
			t.Fatal("duplicate token generated")
		}
		seen[otp.Token] = true
	}
}

func TestInviteRoundTrip(t *testing.T) {
	kp, _ := GenerateKeyPair()
	otp, _ := NewOTP()
	want := Invite{
		Version: 1, HostKey: "hk_abc",
		TailnetDNS: "muxscale-mbp.tail1234.ts.net",
		PublicKey:  kp.PublicB64(), OTP: otp.Token, Port: 4443,
	}

	got, err := ParseInvite(want.InviteURL())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
	}

	// The parsed key must be the daemon's real key.
	pub, err := ParsePublicKey(got.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pub, kp.Public) {
		t.Error("the public key did not survive the invite round trip")
	}
}

// A corrupted scan should be reported as a bad QR code, not surface later as a
// mysterious crypto failure.
func TestInviteRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"wrong scheme":    "https://muxscale.com/pair?v=1&host=h&pk=x&otp=o",
		"wrong host":      "muxscale://join?v=1&host=h&pk=x&otp=o",
		"unknown version": "muxscale://pair?v=99&host=h&pk=AAAA&otp=o",
		"missing key":     "muxscale://pair?v=1&host=h&otp=o",
		"missing otp":     "muxscale://pair?v=1&host=h&pk=AAAA",
		"corrupt key":     "muxscale://pair?v=1&host=h&pk=not-a-key&otp=o",
		"short key":       "muxscale://pair?v=1&host=h&pk=" + base64.RawURLEncoding.EncodeToString([]byte("short")) + "&otp=o",
		"not a url":       "::::",
	}
	for name, raw := range cases {
		if _, err := ParseInvite(raw); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParsePublicKeyAcceptsBothBase64Alphabets(t *testing.T) {
	kp, _ := GenerateKeyPair()
	rawURL := base64.RawURLEncoding.EncodeToString(kp.Public)
	std := base64.StdEncoding.EncodeToString(kp.Public)

	for name, enc := range map[string]string{"raw-url": rawURL, "standard": std} {
		got, err := ParsePublicKey(enc)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, kp.Public) {
			t.Errorf("%s: key mismatch", name)
		}
	}
}

func TestDeviceRevocation(t *testing.T) {
	d := Device{ID: "dev_1", PairedAt: time.Now()}
	if !d.Active() {
		t.Error("a freshly paired device should be active")
	}
	d.RevokedAt = time.Now()
	if d.Active() {
		t.Error("a revoked device must not be active")
	}
}

func TestNewDeviceIDIsPrefixedAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := NewDeviceID()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(id, "dev_") {
			t.Errorf("id %q lacks the dev_ prefix", id)
		}
		if seen[id] {
			t.Fatal("duplicate device id")
		}
		seen[id] = true
	}
}

func TestKeyPairIsValidX25519(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if len(kp.Public) != 32 || len(kp.Private) != 32 {
		t.Fatalf("key sizes = %d/%d, want 32/32", len(kp.Public), len(kp.Private))
	}
	// RFC 7748 clamping.
	if kp.Private[0]&7 != 0 {
		t.Error("private key low bits not cleared")
	}
	if kp.Private[31]&128 != 0 || kp.Private[31]&64 == 0 {
		t.Error("private key high bits not clamped")
	}

	other, _ := GenerateKeyPair()
	if bytes.Equal(kp.Public, other.Public) {
		t.Fatal("two generated keys are identical")
	}
}
