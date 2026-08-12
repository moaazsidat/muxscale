package e2e

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/blake2s"
)

// Pairing.
//
// The threat this guards against is specific: a fully compromised muxscale
// cloud that sits in the middle of the exchange establishing trust. Noise IK
// authenticates the daemon *if the device already has the right public key* --
// so everything depends on that key arriving intact. A QR code scanned off the
// host's own screen is a channel the relay cannot touch, and the short
// authentication string is the fallback that catches it if the key arrived by
// some path that could be tampered with.

// OTPBytes is the size of a one-time enrolment token.
const OTPBytes = 16

// OTPTTL is how long an enrolment token stays valid.
//
// Five minutes is long enough to walk to another room and unlock a phone, and
// short enough that a token left on screen is not a standing invitation.
const OTPTTL = 5 * time.Minute

var otpEnc = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// OTP is a single-use enrolment token.
type OTP struct {
	Token     string
	ShortCode string
	ExpiresAt time.Time
}

// NewOTP mints an enrolment token.
//
// ShortCode is the first 8 characters, for someone typing it instead of
// scanning. It is not the secret: it identifies the pairing attempt so the
// device can fetch the full blob, and the SAS is what actually authenticates.
func NewOTP() (OTP, error) {
	var raw [OTPBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return OTP{}, err
	}
	tok := otpEnc.EncodeToString(raw[:])
	return OTP{
		Token:     tok,
		ShortCode: strings.ToUpper(tok[:8]),
		ExpiresAt: time.Now().Add(OTPTTL),
	}, nil
}

// Expired reports whether the token is past its TTL.
func (o OTP) Expired() bool { return time.Now().After(o.ExpiresAt) }

// Equal compares tokens in constant time.
func (o OTP) Equal(candidate string) bool {
	return subtle.ConstantTimeCompare([]byte(o.Token), []byte(candidate)) == 1
}

// Invite is what a QR code encodes.
type Invite struct {
	Version    int
	HostKey    string // the daemon's stable host identifier
	TailnetDNS string // MagicDNS name, so the phone can try a direct connection
	PublicKey  string // the daemon's X25519 public key, base64
	OTP        string
	Port       int
}

// InviteURL renders an invite as a muxscale:// URL for a QR code.
func (i Invite) InviteURL() string {
	q := url.Values{}
	q.Set("v", fmt.Sprint(i.Version))
	q.Set("host", i.HostKey)
	if i.TailnetDNS != "" {
		q.Set("ts", i.TailnetDNS)
	}
	q.Set("pk", i.PublicKey)
	q.Set("otp", i.OTP)
	if i.Port != 0 {
		q.Set("port", fmt.Sprint(i.Port))
	}
	return "muxscale://pair?" + q.Encode()
}

// ParseInvite decodes a scanned invite.
func ParseInvite(raw string) (Invite, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Invite{}, fmt.Errorf("not a valid invite: %w", err)
	}
	if u.Scheme != "muxscale" || u.Host != "pair" {
		return Invite{}, fmt.Errorf("not a muxscale pairing link")
	}
	q := u.Query()

	inv := Invite{
		HostKey:    q.Get("host"),
		TailnetDNS: q.Get("ts"),
		PublicKey:  q.Get("pk"),
		OTP:        q.Get("otp"),
	}
	fmt.Sscanf(q.Get("v"), "%d", &inv.Version)
	fmt.Sscanf(q.Get("port"), "%d", &inv.Port)

	if inv.Version != 1 {
		return Invite{}, fmt.Errorf("unsupported invite version %d; update the app", inv.Version)
	}
	if inv.HostKey == "" || inv.PublicKey == "" || inv.OTP == "" {
		return Invite{}, fmt.Errorf("invite is missing required fields")
	}
	// Validate the key here rather than at handshake time, so a corrupted scan
	// is reported as a bad QR code and not as a mysterious crypto failure.
	if _, err := ParsePublicKey(inv.PublicKey); err != nil {
		return Invite{}, fmt.Errorf("invite carries an invalid public key: %w", err)
	}
	return inv, nil
}

// sasWords is the word list the short authentication string is drawn from.
//
// Words are short, common, and chosen to be hard to mishear from across a room.
// The list is a power of two so the mapping from bits to words is uniform: a
// modulo over a non-power-of-two list biases early words and quietly reduces
// the entropy the check is supposed to provide.
var sasWords = []string{
	"acid", "acorn", "alarm", "album", "amber", "ankle", "apple", "apron",
	"arrow", "atlas", "aunt", "award", "bacon", "badge", "baker", "banjo",
	"basil", "beach", "beard", "began", "belly", "bench", "berry", "bison",
	"blade", "blank", "blaze", "blend", "blimp", "block", "blush", "board",
	"bonus", "boost", "botch", "brace", "brain", "brass", "bread", "brick",
	"brief", "bring", "brisk", "broke", "brush", "buddy", "buggy", "build",
	"bunch", "bunny", "cabin", "cable", "cache", "camel", "candy", "canoe",
	"cargo", "carve", "catch", "cause", "chalk", "charm", "chase", "check",
	"chess", "chief", "chime", "choir", "chunk", "cider", "cigar", "civic",
	"claim", "clamp", "clash", "clean", "clerk", "cliff", "climb", "cloak",
	"clock", "cloud", "clove", "clown", "coach", "coast", "cobra", "cocoa",
	"comet", "coral", "couch", "cough", "cover", "crane", "crash", "crate",
	"crawl", "creek", "crisp", "crown", "crumb", "crush", "cubic", "curve",
	"cycle", "daisy", "dance", "dandy", "debug", "decoy", "delta", "dense",
	"depot", "devil", "diary", "diner", "dingo", "ditch", "diver", "dizzy",
	"dodge", "donor", "donut", "draft", "drama", "dream", "dress", "drift",
	"drill", "drink", "drive", "drone", "drown", "dwarf", "eagle", "early",
	"earth", "easel", "eaten", "ebony", "eight", "elbow", "elder", "elfin",
	"elite", "elope", "ember", "empty", "enemy", "enjoy", "entry", "equal",
	"erase", "error", "essay", "ether", "event", "every", "exact", "exile",
	"extra", "fable", "faint", "fairy", "faith", "false", "fancy", "fatal",
	"fault", "favor", "feast", "fence", "ferry", "fetch", "fever", "fiber",
	"field", "fiery", "fifth", "fifty", "fight", "final", "finch", "first",
	"flame", "flash", "fleet", "flesh", "flint", "float", "flock", "flood",
	"floor", "flour", "fluid", "flute", "foamy", "focal", "focus", "foggy",
	"force", "forge", "forty", "found", "frame", "fraud", "fresh", "fried",
	"frost", "fruit", "fudge", "fuzzy", "gauge", "gecko", "ghost", "giant",
	"giddy", "girth", "given", "glade", "gland", "glare", "glass", "glaze",
	"gleam", "globe", "gloom", "glory", "glove", "gnome", "goose", "grace",
	"grain", "grand", "grape", "graph", "grasp", "grass", "grave", "gravy",
	"graze", "great", "greed", "green", "greet", "grief", "grill", "grime",
	"grind", "gripe", "groan", "groom", "grove", "growl", "grunt", "guard",
}

// SASWordCount is how many words the short authentication string uses.
//
// Eight words from a 256-word list is 64 bits. That is far more than a human
// comparison needs against an online attacker who gets one attempt, and it
// stays comfortably readable aloud.
const SASWordCount = 8

func init() {
	if len(sasWords) != 256 {
		panic(fmt.Sprintf(
			"the SAS word list must be exactly 256 words for a uniform mapping, got %d",
			len(sasWords)))
	}
}

// SAS derives the short authentication string from a handshake's channel
// binding.
//
// Both sides compute this independently from their own view of the handshake.
// A relay in the middle necessarily runs two different handshakes, so it cannot
// make both sides display the same words -- which is exactly what the human
// comparison detects.
func SAS(channelBinding []byte) ([]string, error) {
	if len(channelBinding) == 0 {
		return nil, fmt.Errorf("no channel binding; the handshake is not complete")
	}
	// Unkeyed hash with an explicit domain-separation prefix. There is no
	// secret to key with here -- both parties derive this from a value they
	// already share -- and a prefix is the honest way to say "this hash is for
	// the SAS and nothing else".
	h, err := blake2s.New256(nil)
	if err != nil {
		return nil, err
	}
	_, _ = h.Write([]byte("muxscale-sas-v1\x00"))
	_, _ = h.Write(channelBinding)
	sum := h.Sum(nil)

	words := make([]string, SASWordCount)
	for i := 0; i < SASWordCount; i++ {
		words[i] = sasWords[sum[i]]
	}
	return words, nil
}

// SASString renders the words for display.
func SASString(words []string) string { return strings.Join(words, " ") }

// SASMatches compares two short authentication strings in constant time.
//
// Constant time is arguably theatre for a value a human reads aloud, but the
// comparison is cheap and the habit is worth keeping: this function could
// later be called on a value an attacker can probe.
func SASMatches(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare(
		[]byte(strings.Join(a, " ")), []byte(strings.Join(b, " "))) == 1
}

// Device is an enrolled device as the daemon records it.
type Device struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Platform     string    `json:"platform"`
	PublicKey    string    `json:"public_key"`
	PairedAt     time.Time `json:"paired_at"`
	SASConfirmed bool      `json:"sas_confirmed"`
	LastSeenAt   time.Time `json:"last_seen_at,omitempty"`
	RevokedAt    time.Time `json:"revoked_at,omitempty"`
}

// Active reports whether the device may connect.
func (d Device) Active() bool { return d.RevokedAt.IsZero() }

// NewDeviceID mints an identifier for an enrolled device.
func NewDeviceID() (string, error) {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "dev_" + otpEnc.EncodeToString(raw[:]), nil
}

// RoutingHeader is the only cleartext a relay ever sees.
//
// It carries what a forwarder genuinely needs and nothing else. It is passed as
// additional authenticated data when sealing, so a relay that rewrites any of
// it makes the payload fail to decrypt rather than silently misdelivering.
type RoutingHeader struct {
	Version  uint8
	HostKey  string
	DeviceID string
	StreamID uint32
}

// headerDomain separates these bytes from any other authenticated string in
// the protocol, so a header can never be confused with a payload that happens
// to look like one.
const headerDomain = "muxscale-hdr-v1\x00"

// Encode renders the header into the bytes bound into the AEAD.
//
// Length-prefixed rather than delimited. The previous encoding was
// fmt.Sprintf("v%d|%s|%s|%d", ...), which is ambiguous whenever an identifier
// can contain the separator:
//
//	{HostKey: "hk_a|dev_b", DeviceID: "dev_c"}  -> "v1|hk_a|dev_b|dev_c|1"
//	{HostKey: "hk_a",       DeviceID: "dev_b|dev_c"} -> "v1|hk_a|dev_b|dev_c|1"
//
// Two different (host, device) pairs producing identical authenticated bytes
// means the AEAD binds the *string* rather than the *tuple*, so a message
// sealed for one pair verifies as the other. Whether that is reachable depends
// on whether an attacker can influence an identifier, which is not a property
// worth depending on when the fix is this cheap.
//
// Rust takes the header as opaque bytes and frames it verbatim, so it needs no
// matching change; the encoding exists only here.
func (h RoutingHeader) Encode() []byte {
	b := make([]byte, 0, len(headerDomain)+1+2+len(h.HostKey)+2+len(h.DeviceID)+4)
	b = append(b, headerDomain...)
	b = append(b, h.Version)
	b = binary.BigEndian.AppendUint16(b, uint16(len(h.HostKey)))
	b = append(b, h.HostKey...)
	b = binary.BigEndian.AppendUint16(b, uint16(len(h.DeviceID)))
	b = append(b, h.DeviceID...)
	b = binary.BigEndian.AppendUint32(b, h.StreamID)
	return b
}

// maxIdentifier bounds the fields whose length is written as a uint16. An
// identifier longer than this would wrap the length prefix and reintroduce the
// ambiguity this encoding exists to remove, so it is rejected at the boundary
// instead.
const maxIdentifier = 1 << 15

// Valid reports whether the header can be encoded unambiguously.
func (h RoutingHeader) Valid() error {
	if len(h.HostKey) > maxIdentifier {
		return fmt.Errorf("host key is %d bytes, over the %d limit", len(h.HostKey), maxIdentifier)
	}
	if len(h.DeviceID) > maxIdentifier {
		return fmt.Errorf("device id is %d bytes, over the %d limit", len(h.DeviceID), maxIdentifier)
	}
	return nil
}

// SealedEnvelope is what crosses the relay.
type SealedEnvelope struct {
	Header     RoutingHeader `json:"header"`
	Ciphertext string        `json:"ciphertext"`
}

// Seal encrypts a payload into an envelope bound to its routing header.
func Seal(s *Session, h RoutingHeader, plaintext []byte) (SealedEnvelope, error) {
	if err := h.Valid(); err != nil {
		return SealedEnvelope{}, fmt.Errorf("routing header: %w", err)
	}
	ct, err := s.Seal(plaintext, h.Encode())
	if err != nil {
		return SealedEnvelope{}, err
	}
	return SealedEnvelope{
		Header:     h,
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
	}, nil
}

// Open decrypts an envelope, verifying it was addressed as received.
func Open(s *Session, env SealedEnvelope) ([]byte, error) {
	ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("envelope ciphertext is not valid base64: %w", err)
	}
	return s.Open(ct, env.Header.Encode())
}
