package relay

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/e2e"
)

// Load characterisation for the relay.
//
// The spec's M5 exit criterion is a 1,000-host load test. That number is about
// the production Durable Object deployment, which needs a Cloudflare account;
// what can be established here is the shape of the cost -- per-room memory,
// forwarding throughput, and behaviour under fan-out and backpressure -- so the
// deployed version has something to be compared against rather than being the
// first measurement anyone takes.
//
// Run with:
//   go test ./internal/relay -run TestLoad -v
//   go test ./internal/relay -bench BenchmarkForward -benchmem

// A host connected 24/7 but quiet for 23 hours is the normal case, so idle
// per-room cost is the number that decides whether the economics work.
func TestLoadIdleRoomFootprint(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load characterisation in short mode")
	}
	const rooms = 1000

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	srv := NewServer(NewStaticAuthorizer(), DefaultLimits(), quietLog())
	conns := make([]*ChanConn, 0, rooms)
	for i := 0; i < rooms; i++ {
		room := srv.Room(fmt.Sprintf("hk_%d", i))
		c := NewChanConn(DefaultLimits().SendQueue)
		room.AttachHost(c)
		conns = append(conns, c)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	perRoom := (after.HeapAlloc - before.HeapAlloc) / rooms
	t.Logf("%d idle rooms: %.1f MB total, ~%d bytes per room",
		rooms, float64(after.HeapAlloc-before.HeapAlloc)/(1<<20), perRoom)

	// The send queue dominates: 256 slots of Frame per connection. This bound
	// is loose on purpose -- it exists to catch an accidental order-of-magnitude
	// regression, not to pin an exact figure.
	if perRoom > 128*1024 {
		t.Errorf("an idle room costs %d bytes, which is more than expected; "+
			"1000 hosts would need %.0f MB just to sit there",
			perRoom, float64(perRoom)*rooms/(1<<20))
	}
	runtime.KeepAlive(conns)
}

// Routing overhead through one room.
//
// This measures the relay's *own* cost -- lock, lookup, counter, channel
// handoff -- and nothing else. Frame bodies are strings, so passing one costs a
// pointer copy, and the numbers here are consequently enormous and not a
// throughput figure for anything real.
//
// That is deliberate and worth stating plainly: the production bottleneck is
// WebSocket framing and the network, neither of which this exercises. What this
// does establish is that routing is nowhere near being the bottleneck, so if
// the deployed relay is slow, the cause is elsewhere.
func TestLoadRoutingOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load characterisation in short mode")
	}
	room := NewRoom("hk_1", DefaultLimits(), quietLog())
	host := NewChanConn(1024)
	room.AttachHost(host)
	_ = room.AttachDevice("dev_1", NewChanConn(1024))

	// Drain the host side as a real daemon would.
	var received atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range host.Out {
			received.Add(1)
		}
	}()

	// A 4 KiB frame is a typical grid delta with a few changed rows.
	body := make([]byte, 4096)
	for i := range body {
		body[i] = 'x'
	}
	frame := Frame{Body: string(body)}

	const n = 50_000
	start := time.Now()
	for i := 0; i < n; i++ {
		if err := room.FromDevice("dev_1", frame); err != nil {
			t.Fatalf("forward %d failed: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	_ = host.Close()
	<-done

	perFrame := elapsed / n
	t.Logf("routed %d frames in %v: %v per frame of relay overhead (%d delivered)",
		n, elapsed.Round(time.Millisecond), perFrame, received.Load())
	t.Log("this is routing cost only -- no serialisation, no network. " +
		"The real ceiling is WebSocket framing and the link, not this.")

	// A microsecond per frame would already be 80x the 12fps-per-session
	// budget for a busy room. The bound is loose because the point is to catch
	// a regression that makes routing matter, not to pin a figure.
	if perFrame > time.Microsecond {
		t.Errorf("relay routing costs %v per frame; it has become a bottleneck", perFrame)
	}
}

// Fan-out to several devices watching one host, which is the phone-plus-tablet
// case.
func TestLoadFanOut(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load characterisation in short mode")
	}
	const devices = 8
	room := NewRoom("hk_1", DefaultLimits(), quietLog())
	room.AttachHost(NewChanConn(64))

	var drained atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < devices; i++ {
		id := fmt.Sprintf("dev_%d", i)
		c := NewChanConn(2048)
		_ = room.AttachDevice(id, c)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range c.Out {
				drained.Add(1)
			}
		}()
	}

	const perDevice = 2000
	start := time.Now()
	for i := 0; i < perDevice; i++ {
		for d := 0; d < devices; d++ {
			_ = room.FromHost(Frame{
				Header: e2e.RoutingHeader{DeviceID: fmt.Sprintf("dev_%d", d)},
				Body:   "delta",
			})
		}
	}
	elapsed := time.Since(start)

	for _, id := range room.DeviceIDs() {
		room.EvictDevice(id)
	}
	wg.Wait()

	total := perDevice * devices
	t.Logf("fanned %d frames to %d devices in %v (%v per frame), %d drained",
		total, devices, elapsed.Round(time.Millisecond), elapsed/time.Duration(total),
		drained.Load())
}

// Concurrent rooms, which is what a shared relay actually looks like. Chiefly a
// race-detector exercise: run with -race.
func TestLoadConcurrentRooms(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load characterisation in short mode")
	}
	const (
		rooms         = 50
		framesPerRoom = 500
	)
	srv := NewServer(NewStaticAuthorizer(), DefaultLimits(), quietLog())

	var wg sync.WaitGroup
	var forwarded atomic.Int64
	start := time.Now()

	for i := 0; i < rooms; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			room := srv.Room(fmt.Sprintf("hk_%d", i))
			host := NewChanConn(1024)
			room.AttachHost(host)
			dev := NewChanConn(1024)
			_ = room.AttachDevice("dev_1", dev)

			go func() {
				for range host.Out {
				}
			}()

			for f := 0; f < framesPerRoom; f++ {
				if err := room.FromDevice("dev_1", Frame{Body: "payload"}); err == nil {
					forwarded.Add(1)
				}
			}
			_ = host.Close()
		}(i)
	}
	wg.Wait()

	t.Logf("%d rooms x %d frames = %d forwarded in %v",
		rooms, framesPerRoom, forwarded.Load(), time.Since(start).Round(time.Millisecond))
	if forwarded.Load() != rooms*framesPerRoom {
		t.Errorf("forwarded %d of %d", forwarded.Load(), rooms*framesPerRoom)
	}
}

// A slow device must never stall the host. This is the property that decides
// whether one bad phone can degrade a whole room, so it is measured rather than
// assumed.
func TestLoadSlowDeviceDoesNotStallTheHost(t *testing.T) {
	limits := DefaultLimits()
	limits.SendQueue = 16
	room := NewRoom("hk_1", limits, quietLog())
	room.AttachHost(NewChanConn(64))

	slow := NewChanConn(16) // never drained
	_ = room.AttachDevice("dev_slow", slow)
	fast := NewChanConn(4096)
	_ = room.AttachDevice("dev_fast", fast)

	var fastReceived atomic.Int64
	go func() {
		for range fast.Out {
			fastReceived.Add(1)
		}
	}()

	const n = 5000
	start := time.Now()
	for i := 0; i < n; i++ {
		_ = room.FromHost(Frame{Header: e2e.RoutingHeader{DeviceID: "dev_slow"}, Body: "x"})
		_ = room.FromHost(Frame{Header: e2e.RoutingHeader{DeviceID: "dev_fast"}, Body: "x"})
	}
	elapsed := time.Since(start)

	t.Logf("%d frames to a stalled device and a healthy one in %v; "+
		"slow dropped %d, fast received %d",
		n*2, elapsed.Round(time.Millisecond), slow.Dropped(), fastReceived.Load())

	if elapsed > 5*time.Second {
		t.Errorf("a stalled device slowed forwarding to %v; it must never block", elapsed)
	}
	if slow.Dropped() == 0 {
		t.Error("the stalled device should have had frames dropped")
	}
}

func BenchmarkForwardDeviceToHost(b *testing.B) {
	room := NewRoom("hk_1", Limits{MaxDevices: 4, SendQueue: 1 << 16, MaxFrameBytes: 4 << 20}, quietLog())
	host := NewChanConn(1 << 16)
	room.AttachHost(host)
	_ = room.AttachDevice("dev_1", NewChanConn(64))
	go func() {
		for range host.Out {
		}
	}()

	body := make([]byte, 4096)
	frame := Frame{Body: string(body)}
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = room.FromDevice("dev_1", frame)
	}
}

func BenchmarkSealThenForward(b *testing.B) {
	daemon, _ := e2e.GenerateKeyPair()
	device, _ := e2e.GenerateKeyPair()
	dhs, _ := e2e.NewHandshake(e2e.RoleDevice, device, daemon.Public)
	hhs, _ := e2e.NewHandshake(e2e.RoleDaemon, daemon, nil)
	m1, _ := dhs.WriteMessage(nil)
	_, _ = hhs.ReadMessage(m1)
	m2, _ := hhs.WriteMessage(nil)
	_, _ = dhs.ReadMessage(m2)
	sess, _ := dhs.Session()

	room := NewRoom("hk_1", Limits{MaxDevices: 4, SendQueue: 1 << 16, MaxFrameBytes: 4 << 20}, quietLog())
	host := NewChanConn(1 << 16)
	room.AttachHost(host)
	_ = room.AttachDevice("dev_1", NewChanConn(64))
	go func() {
		for range host.Out {
		}
	}()

	payload := make([]byte, 4096)
	hdr := e2e.RoutingHeader{Version: 1, HostKey: "hk_1", DeviceID: "dev_1", StreamID: 1}

	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		env, err := e2e.Seal(sess, hdr, payload)
		if err != nil {
			b.Fatal(err)
		}
		_ = room.FromDevice("dev_1", Frame{Header: env.Header, Body: env.Ciphertext})
	}
}
