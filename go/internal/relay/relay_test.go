package relay

import (
	"encoding/base64"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/muxscale/muxscale/go/internal/e2e"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testRoom(t *testing.T) *Room {
	t.Helper()
	return NewRoom("hk_1", DefaultLimits(), quietLog())
}

func TestForwardDeviceToHost(t *testing.T) {
	room := testRoom(t)
	host := NewChanConn(8)
	device := NewChanConn(8)
	room.AttachHost(host)
	if err := room.AttachDevice("dev_1", device); err != nil {
		t.Fatal(err)
	}

	if err := room.FromDevice("dev_1", Frame{Body: "sealed-payload"}); err != nil {
		t.Fatal(err)
	}
	got := <-host.Out
	if got.Body != "sealed-payload" {
		t.Errorf("body = %q", got.Body)
	}
	if got.Header.DeviceID != "dev_1" || got.Header.HostKey != "hk_1" {
		t.Errorf("header = %+v; the relay must stamp the authenticated identity", got.Header)
	}
}

func TestForwardHostToDevice(t *testing.T) {
	room := testRoom(t)
	host := NewChanConn(8)
	a := NewChanConn(8)
	b := NewChanConn(8)
	room.AttachHost(host)
	_ = room.AttachDevice("dev_a", a)
	_ = room.AttachDevice("dev_b", b)

	err := room.FromHost(Frame{
		Header: e2e.RoutingHeader{DeviceID: "dev_b"}, Body: "for-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-b.Out:
		if f.Body != "for-b" {
			t.Errorf("body = %q", f.Body)
		}
	default:
		t.Fatal("dev_b received nothing")
	}
	select {
	case f := <-a.Out:
		t.Fatalf("dev_a received a frame addressed to dev_b: %+v", f)
	default:
	}
}

// A device claiming to be another device is the obvious attack. The sealing
// would catch it downstream, but the relay must not forward the lie at all.
func TestRelayOverwritesSpoofedDeviceID(t *testing.T) {
	room := testRoom(t)
	host := NewChanConn(8)
	room.AttachHost(host)
	_ = room.AttachDevice("dev_honest", NewChanConn(8))

	err := room.FromDevice("dev_honest", Frame{
		Header: e2e.RoutingHeader{DeviceID: "dev_victim"}, Body: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := <-host.Out
	if got.Header.DeviceID != "dev_honest" {
		t.Errorf("device id = %q; the relay must use the authenticated identity, not the claimed one",
			got.Header.DeviceID)
	}
}

// The relay carries sealed bytes. This asserts it, end to end, with a real
// handshake rather than a stand-in.
func TestRelayCannotReadWhatItForwards(t *testing.T) {
	daemonKP, _ := e2e.GenerateKeyPair()
	deviceKP, _ := e2e.GenerateKeyPair()

	deviceHS, _ := e2e.NewHandshake(e2e.RoleDevice, deviceKP, daemonKP.Public)
	daemonHS, _ := e2e.NewHandshake(e2e.RoleDaemon, daemonKP, nil)
	m1, _ := deviceHS.WriteMessage(nil)
	_, _ = daemonHS.ReadMessage(m1)
	m2, _ := daemonHS.WriteMessage(nil)
	_, _ = deviceHS.ReadMessage(m2)

	deviceSess, _ := deviceHS.Session()
	daemonSess, _ := daemonHS.Session()

	secret := "approve deleting the production database"
	hdr := e2e.RoutingHeader{Version: 1, HostKey: "hk_1", DeviceID: "dev_1", StreamID: 1}
	env, err := e2e.Seal(deviceSess, hdr, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}

	room := testRoom(t)
	host := NewChanConn(8)
	room.AttachHost(host)
	_ = room.AttachDevice("dev_1", NewChanConn(8))

	if err := room.FromDevice("dev_1", Frame{Header: env.Header, Body: env.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	forwarded := <-host.Out

	// Everything the relay handled.
	visible := forwarded.Body + string(forwarded.Header.Encode())
	for _, canary := range []string{"approve", "production", "database"} {
		if strings.Contains(strings.ToLower(visible), canary) {
			t.Errorf("the relay could see %q in what it forwarded", canary)
		}
	}
	raw, _ := base64.StdEncoding.DecodeString(forwarded.Body)
	if strings.Contains(strings.ToLower(string(raw)), "database") {
		t.Error("plaintext survived into the forwarded bytes")
	}

	// And the host can still read it.
	got, err := e2e.Open(daemonSess, e2e.SealedEnvelope{
		Header: forwarded.Header, Ciphertext: forwarded.Body,
	})
	if err != nil {
		t.Fatalf("the host could not open a frame the relay forwarded: %v", err)
	}
	if string(got) != secret {
		t.Errorf("payload = %q", got)
	}
}

func TestHostOfflineIsDistinguishable(t *testing.T) {
	room := testRoom(t)
	_ = room.AttachDevice("dev_1", NewChanConn(8))
	err := room.FromDevice("dev_1", Frame{Body: "x"})
	if err != ErrHostOffline {
		t.Errorf("err = %v, want ErrHostOffline so a client can say why", err)
	}
}

func TestDeviceOfflineIsDistinguishable(t *testing.T) {
	room := testRoom(t)
	room.AttachHost(NewChanConn(8))
	err := room.FromHost(Frame{Header: e2e.RoutingHeader{DeviceID: "dev_absent"}})
	if err != ErrDeviceOffline {
		t.Errorf("err = %v, want ErrDeviceOffline", err)
	}
}

// A daemon that restarted is the same host and must not be locked out by its
// own stale connection.
func TestReconnectingHostReplacesStaleConnection(t *testing.T) {
	room := testRoom(t)
	first := NewChanConn(8)
	room.AttachHost(first)
	second := NewChanConn(8)
	room.AttachHost(second)

	_ = room.AttachDevice("dev_1", NewChanConn(8))
	if err := room.FromDevice("dev_1", Frame{Body: "after-restart"}); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-second.Out:
		if f.Body != "after-restart" {
			t.Errorf("body = %q", f.Body)
		}
	default:
		t.Fatal("the new host connection received nothing")
	}
	if _, open := <-first.Out; open {
		t.Error("the stale host connection should have been closed")
	}
}

// One slow phone must not stall the host. Dropping is the correct outcome:
// the client repairs by requesting a fresh snapshot.
func TestSlowDeviceIsDroppedNotBlocking(t *testing.T) {
	limits := DefaultLimits()
	limits.SendQueue = 2
	room := NewRoom("hk_1", limits, quietLog())
	room.AttachHost(NewChanConn(8))

	slow := NewChanConn(2)
	_ = room.AttachDevice("dev_slow", slow)

	// Far more frames than the queue can hold. None of these may block.
	for i := 0; i < 100; i++ {
		if err := room.FromHost(Frame{
			Header: e2e.RoutingHeader{DeviceID: "dev_slow"}, Body: "frame",
		}); err != nil {
			t.Fatalf("forwarding blocked or errored at frame %d: %v", i, err)
		}
	}
	if slow.Dropped() == 0 {
		t.Error("expected frames to be dropped under backpressure")
	}
}

func TestFrameSizeLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxFrameBytes = 16
	room := NewRoom("hk_1", limits, quietLog())
	room.AttachHost(NewChanConn(8))
	_ = room.AttachDevice("dev_1", NewChanConn(8))

	big := strings.Repeat("x", 64)
	if err := room.FromDevice("dev_1", Frame{Body: big}); err != ErrFrameTooLarge {
		t.Errorf("err = %v, want ErrFrameTooLarge", err)
	}
	if _, err := ParseFrame([]byte(`{"body":"`+big+`"}`), 16); err != ErrFrameTooLarge {
		t.Errorf("ParseFrame err = %v, want ErrFrameTooLarge", err)
	}
}

func TestDeviceLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxDevices = 2
	room := NewRoom("hk_1", limits, quietLog())

	if err := room.AttachDevice("d1", NewChanConn(4)); err != nil {
		t.Fatal(err)
	}
	if err := room.AttachDevice("d2", NewChanConn(4)); err != nil {
		t.Fatal(err)
	}
	if err := room.AttachDevice("d3", NewChanConn(4)); err != ErrTooManyDevices {
		t.Errorf("err = %v, want ErrTooManyDevices", err)
	}
	// Reconnecting an existing device must still be allowed at the limit.
	if err := room.AttachDevice("d1", NewChanConn(4)); err != nil {
		t.Errorf("an existing device could not reconnect at the limit: %v", err)
	}
}

// Revocation has to work while the host is offline, which is why the relay
// enforces it independently rather than deferring to the daemon.
func TestEvictDeviceWorksWithHostOffline(t *testing.T) {
	room := testRoom(t)
	c := NewChanConn(4)
	_ = room.AttachDevice("dev_bad", c)

	if room.HostOnline() {
		t.Fatal("host should be offline for this test")
	}
	if !room.EvictDevice("dev_bad") {
		t.Fatal("eviction reported no such device")
	}
	if _, open := <-c.Out; open {
		t.Error("the evicted device's connection should be closed")
	}
	if len(room.DeviceIDs()) != 0 {
		t.Error("the evicted device is still listed")
	}
}

func TestDetachOnlyRemovesTheCurrentConnection(t *testing.T) {
	room := testRoom(t)
	first := NewChanConn(4)
	_ = room.AttachDevice("dev_1", first)
	second := NewChanConn(4)
	_ = room.AttachDevice("dev_1", second)

	// A late detach from the replaced connection must not evict the live one.
	room.DetachDevice("dev_1", first)
	if len(room.DeviceIDs()) != 1 {
		t.Error("a stale detach removed the current connection")
	}
	room.DetachDevice("dev_1", second)
	if len(room.DeviceIDs()) != 0 {
		t.Error("detaching the current connection did not remove it")
	}
}

// ---- authorization ----

func TestStaticAuthorizer(t *testing.T) {
	a := NewStaticAuthorizer()
	a.SetHostToken("hk_1", "secret")
	a.LinkDevice("hk_1", "dev_1")

	if err := a.AuthorizeHost("hk_1", "secret"); err != nil {
		t.Errorf("valid host token rejected: %v", err)
	}
	if err := a.AuthorizeHost("hk_1", "wrong"); err == nil {
		t.Error("a wrong host token was accepted")
	}
	if err := a.AuthorizeHost("hk_unknown", "secret"); err == nil {
		t.Error("an unknown host was accepted")
	}

	if err := a.AuthorizeDevice("hk_1", "dev_1", ""); err != nil {
		t.Errorf("a paired device was rejected: %v", err)
	}
	if err := a.AuthorizeDevice("hk_1", "dev_other", ""); err == nil {
		t.Error("an unpaired device was accepted")
	}
	if err := a.AuthorizeDevice("hk_other", "dev_1", ""); err == nil {
		t.Error("a device paired to a different host was accepted")
	}
}

// A revoked device must be refused even though it is still paired.
func TestRevokedDeviceIsRefused(t *testing.T) {
	a := NewStaticAuthorizer()
	a.SetHostToken("hk_1", "secret")
	a.LinkDevice("hk_1", "dev_1")
	a.Revoke("dev_1")

	err := a.AuthorizeDevice("hk_1", "dev_1", "")
	if err == nil {
		t.Fatal("a revoked device was authorized")
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("the refusal should say why: %v", err)
	}
}

func TestServerRoomsAreStableAndPerHost(t *testing.T) {
	s := NewServer(NewStaticAuthorizer(), DefaultLimits(), quietLog())
	a1 := s.Room("hk_a")
	a2 := s.Room("hk_a")
	b := s.Room("hk_b")

	if a1 != a2 {
		t.Error("the same host key must return the same room")
	}
	if a1 == b {
		t.Error("different hosts must not share a room")
	}
	if len(s.Rooms()) != 2 {
		t.Errorf("rooms = %v", s.Rooms())
	}
}

func TestServerAuthorizeRejectsUnknownRole(t *testing.T) {
	s := NewServer(NewStaticAuthorizer(), DefaultLimits(), quietLog())
	if err := s.Authorize(Role("hacker"), "hk", "dev", "cred"); err != ErrUnknownRole {
		t.Errorf("err = %v, want ErrUnknownRole", err)
	}
}

// The relay counts bytes for quotas. It must never need content to do so.
func TestBytesForwardedCountsWithoutInspecting(t *testing.T) {
	room := testRoom(t)
	room.AttachHost(NewChanConn(8))
	_ = room.AttachDevice("dev_1", NewChanConn(8))

	body := strings.Repeat("x", 100)
	for i := 0; i < 3; i++ {
		_ = room.FromDevice("dev_1", Frame{Body: body})
	}
	if got := room.BytesForwarded(); got != 300 {
		t.Errorf("bytes = %d, want 300", got)
	}
}

func TestParseFrameRejectsGarbage(t *testing.T) {
	if _, err := ParseFrame([]byte("not json"), 1024); err == nil {
		t.Error("malformed JSON should be rejected")
	}
	f, err := ParseFrame([]byte(`{"header":{"Version":1,"DeviceID":"d"},"body":"abc"}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if f.Body != "abc" || f.Header.DeviceID != "d" {
		t.Errorf("frame = %+v", f)
	}
}
