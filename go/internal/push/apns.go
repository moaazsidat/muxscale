package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// APNs delivery.
//
// Token-based authentication with a .p8 signing key, rather than certificates:
// one key covers every app and never expires, so there is no annual outage
// waiting to happen.
//
// Everything here except the network call is testable without Apple
// credentials, and is tested: key parsing, ES256 JWT signing, token caching and
// refresh, payload shape, and status-code interpretation. What cannot be
// verified without a real .p8 and a real device is that Apple accepts it. That
// gap is stated rather than papered over.

const (
	apnsProduction = "https://api.push.apple.com"
	apnsSandbox    = "https://api.sandbox.push.apple.com"

	// apnsTokenLifetime is how long a provider JWT is reused. Apple rejects
	// tokens older than an hour and also rejects regenerating them too often,
	// so the refresh window sits comfortably between the two.
	apnsTokenLifetime = 45 * time.Minute
)

// APNsConfig configures the transport.
type APNsConfig struct {
	// KeyID is the .p8 key identifier from the Apple Developer portal.
	KeyID string
	// TeamID is the Apple Developer team identifier.
	TeamID string
	// BundleID is the app's bundle identifier, sent as apns-topic.
	BundleID string
	// PrivateKeyPEM is the contents of the .p8 file.
	PrivateKeyPEM []byte
	// Sandbox targets the development APNs host.
	Sandbox bool
	// HTTPClient allows injecting a client; APNs requires HTTP/2.
	HTTPClient *http.Client
}

// APNs is a token-authenticated APNs transport.
type APNs struct {
	cfg    APNsConfig
	key    *ecdsa.PrivateKey
	client *http.Client
	host   string

	mu       sync.Mutex
	token    string
	tokenAge time.Time
}

// NewAPNs builds a transport from a .p8 key.
func NewAPNs(cfg APNsConfig) (*APNs, error) {
	if cfg.KeyID == "" || cfg.TeamID == "" || cfg.BundleID == "" {
		return nil, fmt.Errorf("apns: KeyID, TeamID and BundleID are all required")
	}
	key, err := ParseP8(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	client := cfg.HTTPClient
	if client == nil {
		// APNs requires HTTP/2. Go's default transport negotiates it over TLS,
		// so this is mostly about the timeout.
		client = &http.Client{Timeout: 20 * time.Second}
	}
	host := apnsProduction
	if cfg.Sandbox {
		host = apnsSandbox
	}
	return &APNs{cfg: cfg, key: key, client: client, host: host}, nil
}

// ParseP8 decodes an Apple .p8 ECDSA private key.
func ParseP8(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	if len(pemBytes) == 0 {
		return nil, fmt.Errorf("apns: no private key provided")
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("apns: key is not valid PEM (is this the .p8 file?)")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apns: parsing the .p8 key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("apns: the .p8 key is %T, want an ECDSA key", parsed)
	}
	return key, nil
}

// providerToken returns a cached or freshly signed JWT.
func (a *APNs) providerToken(now time.Time) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && now.Sub(a.tokenAge) < apnsTokenLifetime {
		return a.token, nil
	}
	tok, err := signES256JWT(a.key, a.cfg.KeyID, a.cfg.TeamID, now)
	if err != nil {
		return "", err
	}
	a.token, a.tokenAge = tok, now
	return tok, nil
}

// signES256JWT builds the provider authentication token Apple expects.
func signES256JWT(key *ecdsa.PrivateKey, keyID, teamID string, now time.Time) (string, error) {
	header := map[string]string{"alg": "ES256", "kid": keyID, "typ": "JWT"}
	claims := map[string]any{"iss": teamID, "iat": now.Unix()}

	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := b64(hb) + "." + b64(cb)

	digest := sha256Sum([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	// JWS wants fixed-width big-endian r and s, not an ASN.1 sequence. Getting
	// this wrong produces a token Apple rejects with an opaque error.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	return signing + "." + b64(sig), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// apnsPayload is the JSON body APNs receives.
type apnsPayload struct {
	APS      aps    `json:"aps"`
	Muxscale string `json:"muxscale,omitempty"` // the sealed blob
	Session  string `json:"session_id,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

type aps struct {
	Alert             *apsAlert `json:"alert,omitempty"`
	Sound             string    `json:"sound,omitempty"`
	Category          string    `json:"category,omitempty"`
	ThreadID          string    `json:"thread-id,omitempty"`
	MutableContent    int       `json:"mutable-content,omitempty"`
	InterruptionLevel string    `json:"interruption-level,omitempty"`
}

type apsAlert struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

// BuildAPNsPayload renders the JSON body for a notification.
//
// mutable-content is always 1 so the notification service extension runs and
// can replace the minimal cloud text with the decrypted version. Without it the
// user would only ever see "Waiting for you", however much detail they chose.
func BuildAPNsPayload(n Notification, sealed string) ([]byte, error) {
	p := apnsPayload{
		APS: aps{
			Alert:          &apsAlert{Title: n.Alert.Title, Body: n.Alert.Body},
			Sound:          "default",
			ThreadID:       n.CollapseKey,
			MutableContent: 1,
			Category:       apnsCategory(n.Kind),
		},
		Muxscale: sealed,
		Session:  n.SessionID,
		Kind:     string(n.Kind),
	}
	// time-sensitive cuts through Focus without being a critical alert, which
	// would need a separate Apple entitlement and is not warranted here.
	if n.Kind == KindApproval {
		p.APS.InterruptionLevel = "time-sensitive"
	}
	return json.Marshal(p)
}

func apnsCategory(k Kind) string {
	switch k {
	case KindApproval:
		return "MUXSCALE_APPROVAL"
	case KindCompleted:
		return "MUXSCALE_COMPLETED"
	default:
		return "MUXSCALE_WAITING"
	}
}

// Send delivers a notification to one device token.
func (a *APNs) Send(n Notification, tok Token) Result {
	sealed := n.Sealed[tok.DeviceID]
	body, err := BuildAPNsPayload(n, sealed)
	if err != nil {
		return Result{Err: err}
	}

	jwt, err := a.providerToken(time.Now())
	if err != nil {
		return Result{Err: err}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	url := a.host + "/3/device/" + tok.Token
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Result{Err: err}
	}
	req.Header.Set("authorization", "bearer "+jwt)
	req.Header.Set("apns-topic", a.cfg.BundleID)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	// Collapse by session so a chatty session replaces rather than stacks.
	req.Header.Set("apns-collapse-id", truncateForHeader(n.CollapseKey, 64))
	req.Header.Set("content-type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return Result{Err: fmt.Errorf("apns request: %w", err)}
	}
	defer resp.Body.Close()

	return interpretAPNsStatus(resp.StatusCode, readLimited(resp.Body, 4096))
}

// interpretAPNsStatus maps a response onto a Result.
//
// 410 means the token is dead and must be pruned; retrying it forever is how a
// push backend accumulates permanent garbage. 429 and 5xx are transient and
// worth retrying. Everything else is a configuration error the operator needs
// to see rather than a delivery blip.
func interpretAPNsStatus(status int, body string) Result {
	switch {
	case status == http.StatusOK:
		return Result{OK: true}
	case status == http.StatusGone:
		return Result{Unregistered: true, Err: fmt.Errorf("apns 410: token is no longer valid")}
	case status == http.StatusBadRequest && strings.Contains(body, "BadDeviceToken"):
		return Result{Unregistered: true, Err: fmt.Errorf("apns 400 BadDeviceToken")}
	case status == http.StatusTooManyRequests:
		return Result{Err: fmt.Errorf("apns 429: rate limited, retry later")}
	case status >= 500:
		return Result{Err: fmt.Errorf("apns %d: transient server error", status)}
	default:
		return Result{Err: fmt.Errorf("apns %d: %s", status, strings.TrimSpace(body))}
	}
}

func truncateForHeader(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
