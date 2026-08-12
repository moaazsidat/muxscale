package e2e

import (
	"strings"
	"testing"
)

// The routing header is bound into the AEAD, so its encoding has to map
// distinct headers to distinct bytes. The original delimited form did not, and
// these are the cases that proved it.

func TestRoutingHeaderEncodingIsUnambiguous(t *testing.T) {
	// Each pair is two genuinely different headers that the previous
	// "v%d|%s|%s|%d" encoding rendered identically, by shifting where the
	// separator was taken to fall.
	pairs := [][2]RoutingHeader{
		{
			{Version: 1, HostKey: "hk_a|dev_b", DeviceID: "dev_c", StreamID: 1},
			{Version: 1, HostKey: "hk_a", DeviceID: "dev_b|dev_c", StreamID: 1},
		},
		{
			{Version: 1, HostKey: "", DeviceID: "a", StreamID: 2},
			{Version: 1, HostKey: "a", DeviceID: "", StreamID: 2},
		},
		{
			{Version: 1, HostKey: "x|", DeviceID: "y", StreamID: 3},
			{Version: 1, HostKey: "x", DeviceID: "|y", StreamID: 3},
		},
	}
	for i, p := range pairs {
		a, b := string(p[0].Encode()), string(p[1].Encode())
		if a == b {
			t.Errorf("pair %d: distinct headers encode identically\n  %+v\n  %+v\n  -> %q",
				i, p[0], p[1], a)
		}
	}
}

// Every field must actually be covered: a field left out of the encoding is a
// field a relay could rewrite without invalidating the payload.
func TestEveryHeaderFieldChangesTheEncoding(t *testing.T) {
	base := RoutingHeader{Version: 1, HostKey: "hk_1", DeviceID: "dev_1", StreamID: 9}
	variants := map[string]RoutingHeader{
		"version":  {Version: 2, HostKey: "hk_1", DeviceID: "dev_1", StreamID: 9},
		"hostkey":  {Version: 1, HostKey: "hk_2", DeviceID: "dev_1", StreamID: 9},
		"deviceid": {Version: 1, HostKey: "hk_1", DeviceID: "dev_2", StreamID: 9},
		"streamid": {Version: 1, HostKey: "hk_1", DeviceID: "dev_1", StreamID: 10},
	}
	want := string(base.Encode())
	for field, v := range variants {
		if string(v.Encode()) == want {
			t.Errorf("changing %s did not change the encoding, so a relay could "+
				"rewrite it without breaking authentication", field)
		}
	}
}

func TestHeaderEncodingIsDomainSeparated(t *testing.T) {
	h := RoutingHeader{Version: 1, HostKey: "hk", DeviceID: "dev", StreamID: 1}
	if !strings.HasPrefix(string(h.Encode()), headerDomain) {
		t.Error("the encoding must be domain-separated so header bytes cannot " +
			"collide with any other authenticated string in the protocol")
	}
}

// pairedSessions runs a handshake and returns both ends.
func pairedSessions(t *testing.T) (device, daemon *Session) {
	t.Helper()
	daemonKP, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	deviceKP, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	dev, err := NewHandshake(RoleDevice, deviceKP, daemonKP.Public)
	if err != nil {
		t.Fatal(err)
	}
	dmn, err := NewHandshake(RoleDaemon, daemonKP, nil)
	if err != nil {
		t.Fatal(err)
	}
	m1, err := dev.WriteMessage(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dmn.ReadMessage(m1); err != nil {
		t.Fatal(err)
	}
	m2, err := dmn.WriteMessage(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dev.ReadMessage(m2); err != nil {
		t.Fatal(err)
	}
	device, err = dev.Session()
	if err != nil {
		t.Fatal(err)
	}
	daemon, err = dmn.Session()
	if err != nil {
		t.Fatal(err)
	}
	return device, daemon
}

func TestSealRejectsUnencodableHeaders(t *testing.T) {
	sess, _ := pairedSessions(t)
	h := RoutingHeader{
		Version:  1,
		HostKey:  strings.Repeat("a", maxIdentifier+1),
		DeviceID: "dev_1",
	}
	if _, err := Seal(sess, h, []byte("x")); err == nil {
		t.Fatal("an identifier long enough to wrap the uint16 length prefix must " +
			"be rejected, not silently truncated back into ambiguity")
	}
}

// A message sealed for one header must not open under another, which is the
// property the encoding exists to guarantee.
func TestSealedMessageDoesNotOpenUnderAShiftedHeader(t *testing.T) {
	send, recv := pairedSessions(t)

	a := RoutingHeader{Version: 1, HostKey: "hk_a|dev_b", DeviceID: "dev_c", StreamID: 1}
	b := RoutingHeader{Version: 1, HostKey: "hk_a", DeviceID: "dev_b|dev_c", StreamID: 1}

	env, err := Seal(send, a, []byte("approve the deploy"))
	if err != nil {
		t.Fatal(err)
	}
	env.Header = b // a relay claiming the message was addressed differently
	if _, err := Open(recv, env); err == nil {
		t.Fatal("a message sealed for one (host, device) pair opened under another")
	}
}
