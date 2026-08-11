// Package relay implements the rendezvous that carries traffic when a phone
// cannot reach the daemon directly over the tailnet.
//
// This Go implementation is the reference and the test harness. Production runs
// the same protocol as a Cloudflare Durable Object (see cloud/relay), one per
// host, using WebSocket Hibernation so a daemon connected 24/7 but quiet for 23
// hours costs nothing. Having both means the protocol can be tested end to end
// locally, without deploying anything.
//
// The relay's job is deliberately small: authenticate both ends, check the
// device is still permitted, and forward opaque frames. It cannot read what it
// carries, cannot originate messages, and cannot forge one -- those are
// properties of the sealing in package e2e, not of anything here.
package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/muxscale/muxscale/go/internal/e2e"
)

// Frame is what crosses the relay. The header routes it; the body is sealed.
type Frame struct {
	Header e2e.RoutingHeader `json:"header"`
	Body   string            `json:"body"`
}

// Role identifies which end of a relay connection a peer is.
type Role string

const (
	RoleHost   Role = "host"
	RoleDevice Role = "device"
)

// Conn is one connected peer.
type Conn interface {
	Send(Frame) error
	Close() error
}

// Authorizer decides whether a connection may join a host's room.
//
// This is the relay's second, independent gate. The daemon keeps its own
// device allowlist, but a revoked device must be cut off even while the host
// is offline and cannot enforce anything -- so the relay checks too.
type Authorizer interface {
	// AuthorizeHost validates a host token.
	AuthorizeHost(hostKey, token string) error
	// AuthorizeDevice validates a device ticket and confirms the device is
	// still paired to the host and not revoked.
	AuthorizeDevice(hostKey, deviceID, ticket string) error
}

// Limits bound a room's resource use.
type Limits struct {
	// MaxDevices per host.
	MaxDevices int
	// SendQueue is how many frames may be buffered for one peer.
	SendQueue int
	// MaxFrameBytes rejects oversized frames before they are buffered.
	MaxFrameBytes int
}

// DefaultLimits are conservative and per-host.
func DefaultLimits() Limits {
	return Limits{MaxDevices: 16, SendQueue: 256, MaxFrameBytes: 4 << 20}
}

// Room is one host and its connected devices.
type Room struct {
	HostKey string

	mu      sync.RWMutex
	host    Conn
	devices map[string]Conn
	limits  Limits
	log     *slog.Logger

	// bytesForwarded is metadata the relay legitimately needs for quotas. It
	// counts bytes, never content.
	bytesForwarded int64
}

// NewRoom builds an empty room.
func NewRoom(hostKey string, limits Limits, log *slog.Logger) *Room {
	if log == nil {
		log = slog.Default()
	}
	return &Room{
		HostKey: hostKey, devices: map[string]Conn{},
		limits: limits, log: log,
	}
}

// Errors a caller can distinguish.
var (
	ErrHostOffline    = errors.New("host is not connected")
	ErrDeviceOffline  = errors.New("device is not connected")
	ErrTooManyDevices = errors.New("too many devices connected to this host")
	ErrFrameTooLarge  = errors.New("frame exceeds the size limit")
	ErrUnknownRole    = errors.New("unknown role")
)

// AttachHost connects the daemon. A second host connection replaces the first,
// because a daemon that restarted is the same host and should not be locked out
// by its own stale connection.
func (r *Room) AttachHost(c Conn) {
	r.mu.Lock()
	old := r.host
	r.host = c
	r.mu.Unlock()

	if old != nil {
		_ = old.Close()
		r.log.Info("replaced a stale host connection", "host", r.HostKey)
	}
}

// AttachDevice connects a device.
func (r *Room) AttachDevice(deviceID string, c Conn) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.devices[deviceID]; !exists && len(r.devices) >= r.limits.MaxDevices {
		return ErrTooManyDevices
	}
	if old, exists := r.devices[deviceID]; exists {
		_ = old.Close()
	}
	r.devices[deviceID] = c
	return nil
}

// DetachHost removes the daemon connection if it is the current one.
func (r *Room) DetachHost(c Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.host == c {
		r.host = nil
	}
}

// DetachDevice removes a device connection if it is the current one.
func (r *Room) DetachDevice(deviceID string, c Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.devices[deviceID]; ok && cur == c {
		delete(r.devices, deviceID)
	}
}

// HostOnline reports whether the daemon is connected.
func (r *Room) HostOnline() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.host != nil
}

// DeviceIDs lists connected devices.
func (r *Room) DeviceIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.devices))
	for id := range r.devices {
		out = append(out, id)
	}
	return out
}

// BytesForwarded is the room's traffic counter, for quotas.
func (r *Room) BytesForwarded() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bytesForwarded
}

// FromDevice forwards a device's frame to the host.
//
// The relay overwrites the frame's DeviceID with the authenticated one rather
// than trusting what the device sent. A device claiming to be another device is
// the obvious attack, and the sealing would catch it -- but the relay should
// not forward a lie in the first place.
func (r *Room) FromDevice(deviceID string, f Frame) error {
	if len(f.Body) > r.limits.MaxFrameBytes {
		return ErrFrameTooLarge
	}
	f.Header.DeviceID = deviceID
	f.Header.HostKey = r.HostKey

	r.mu.Lock()
	host := r.host
	r.bytesForwarded += int64(len(f.Body))
	r.mu.Unlock()

	if host == nil {
		return ErrHostOffline
	}
	return host.Send(f)
}

// FromHost forwards the host's frame to the addressed device.
func (r *Room) FromHost(f Frame) error {
	if len(f.Body) > r.limits.MaxFrameBytes {
		return ErrFrameTooLarge
	}
	f.Header.HostKey = r.HostKey

	r.mu.Lock()
	target := r.devices[f.Header.DeviceID]
	r.bytesForwarded += int64(len(f.Body))
	r.mu.Unlock()

	if target == nil {
		return ErrDeviceOffline
	}
	return target.Send(f)
}

// EvictDevice disconnects a device, used when a revocation arrives.
//
// Revocation has to work even while the host is offline, which is why the relay
// enforces it independently rather than deferring to the daemon.
func (r *Room) EvictDevice(deviceID string) bool {
	r.mu.Lock()
	c, ok := r.devices[deviceID]
	delete(r.devices, deviceID)
	r.mu.Unlock()
	if ok {
		_ = c.Close()
		r.log.Info("evicted a device", "host", r.HostKey, "device", deviceID)
	}
	return ok
}

// ---- server ----

// Server holds rooms keyed by host.
type Server struct {
	mu     sync.RWMutex
	rooms  map[string]*Room
	auth   Authorizer
	limits Limits
	log    *slog.Logger
}

// NewServer builds a relay server.
func NewServer(auth Authorizer, limits Limits, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{rooms: map[string]*Room{}, auth: auth, limits: limits, log: log}
}

// Room returns a host's room, creating it if needed.
func (s *Server) Room(hostKey string) *Room {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rooms[hostKey]; ok {
		return r
	}
	r := NewRoom(hostKey, s.limits, s.log)
	s.rooms[hostKey] = r
	return r
}

// Rooms lists active host keys.
func (s *Server) Rooms() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.rooms))
	for k := range s.rooms {
		out = append(out, k)
	}
	return out
}

// Authorize checks a connection request.
func (s *Server) Authorize(role Role, hostKey, deviceID, credential string) error {
	switch role {
	case RoleHost:
		return s.auth.AuthorizeHost(hostKey, credential)
	case RoleDevice:
		return s.auth.AuthorizeDevice(hostKey, deviceID, credential)
	default:
		return ErrUnknownRole
	}
}

// ---- a channel-backed connection, for tests and the in-process relay ----

// ChanConn is a Conn backed by a buffered channel.
type ChanConn struct {
	Out chan Frame

	mu     sync.Mutex
	closed bool
	// dropped counts frames shed under backpressure.
	dropped int
}

// NewChanConn builds a connection with the given queue depth.
func NewChanConn(queue int) *ChanConn {
	if queue <= 0 {
		queue = 64
	}
	return &ChanConn{Out: make(chan Frame, queue)}
}

// Send queues a frame.
//
// A full queue drops the frame rather than blocking. Blocking here would let
// one slow phone stall the host's forwarding loop, which is a far worse outcome
// than that phone missing a frame it can re-request with a fresh snapshot.
func (c *ChanConn) Send(f Frame) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("connection is closed")
	}
	c.mu.Unlock()

	select {
	case c.Out <- f:
		return nil
	default:
		c.mu.Lock()
		c.dropped++
		c.mu.Unlock()
		return nil
	}
}

// Dropped reports how many frames were shed under backpressure.
func (c *ChanConn) Dropped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// Close shuts the connection.
func (c *ChanConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.Out)
	return nil
}

// ---- HTTP surface ----

// StaticAuthorizer is a simple Authorizer for local testing and self-hosting.
type StaticAuthorizer struct {
	mu          sync.RWMutex
	HostTokens  map[string]string              // hostKey -> token
	DeviceLinks map[string]map[string]struct{} // hostKey -> deviceIDs
	Revoked     map[string]struct{}            // deviceID
}

// NewStaticAuthorizer builds an empty authorizer.
func NewStaticAuthorizer() *StaticAuthorizer {
	return &StaticAuthorizer{
		HostTokens:  map[string]string{},
		DeviceLinks: map[string]map[string]struct{}{},
		Revoked:     map[string]struct{}{},
	}
}

// SetHostToken registers a host credential.
func (a *StaticAuthorizer) SetHostToken(hostKey, token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.HostTokens[hostKey] = token
}

// LinkDevice pairs a device to a host.
func (a *StaticAuthorizer) LinkDevice(hostKey, deviceID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.DeviceLinks[hostKey] == nil {
		a.DeviceLinks[hostKey] = map[string]struct{}{}
	}
	a.DeviceLinks[hostKey][deviceID] = struct{}{}
}

// Revoke marks a device as revoked everywhere.
func (a *StaticAuthorizer) Revoke(deviceID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Revoked[deviceID] = struct{}{}
}

// AuthorizeHost validates a host token.
func (a *StaticAuthorizer) AuthorizeHost(hostKey, token string) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	want, ok := a.HostTokens[hostKey]
	if !ok || want == "" || want != token {
		return errors.New("host token rejected")
	}
	return nil
}

// AuthorizeDevice validates a device and checks revocation.
func (a *StaticAuthorizer) AuthorizeDevice(hostKey, deviceID, _ string) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if _, revoked := a.Revoked[deviceID]; revoked {
		return errors.New("device has been revoked")
	}
	links, ok := a.DeviceLinks[hostKey]
	if !ok {
		return errors.New("no devices are paired to this host")
	}
	if _, paired := links[deviceID]; !paired {
		return errors.New("device is not paired to this host")
	}
	return nil
}

// Status is what the relay reports about itself. Metadata only: which hosts
// are connected and how much traffic they moved, never any content.
type Status struct {
	Hosts []HostStatus `json:"hosts"`
}

// HostStatus describes one room.
type HostStatus struct {
	HostKey        string   `json:"host_key"`
	HostOnline     bool     `json:"host_online"`
	Devices        []string `json:"devices"`
	BytesForwarded int64    `json:"bytes_forwarded"`
}

// StatusHandler serves relay status.
func (s *Server) StatusHandler(w http.ResponseWriter, r *http.Request) {
	var out Status
	for _, key := range s.Rooms() {
		room := s.Room(key)
		out.Hosts = append(out.Hosts, HostStatus{
			HostKey:        key,
			HostOnline:     room.HostOnline(),
			Devices:        room.DeviceIDs(),
			BytesForwarded: room.BytesForwarded(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ParseFrame decodes a wire frame, rejecting oversized input first.
func ParseFrame(data []byte, maxBytes int) (Frame, error) {
	if maxBytes > 0 && len(data) > maxBytes {
		return Frame{}, ErrFrameTooLarge
	}
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return Frame{}, fmt.Errorf("malformed frame: %w", err)
	}
	return f, nil
}

// HeartbeatInterval is how often an idle connection is pinged, so a dead peer
// is noticed rather than lingering as a phantom online host.
const HeartbeatInterval = 25 * time.Second
