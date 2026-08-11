package push

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// FCM v1 delivery, for Android.
//
// The legacy server-key API is gone, so this uses FCM v1: a service-account
// JSON key, an RS256 JWT exchanged for an OAuth access token, and a POST to
// the v1 send endpoint.
//
// Messages are **data-only**, deliberately. A notification-payload message is
// rendered by the system before the app runs, which would mean the lock screen
// could only ever show the minimal cloud text -- the sealed content would never
// be decrypted. Data-only wakes FirebaseMessagingService, which decrypts and
// posts the real notification. It is the Android counterpart of iOS's
// mutable-content.

const (
	fcmTokenURL   = "https://oauth2.googleapis.com/token"
	fcmSendFormat = "https://fcm.googleapis.com/v1/projects/%s/messages:send"
	fcmScope      = "https://www.googleapis.com/auth/firebase.messaging"

	// Google's access tokens last an hour; refreshing at 50 minutes avoids
	// both expiry races and needless token churn.
	fcmTokenLifetime = 50 * time.Minute
)

// ServiceAccount is the subset of a Firebase service-account JSON key that
// matters here.
type ServiceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

// FCMConfig configures the transport.
type FCMConfig struct {
	// ServiceAccountJSON is the downloaded key file's contents.
	ServiceAccountJSON []byte
	HTTPClient         *http.Client
	// TokenURL overrides the OAuth endpoint, for tests.
	TokenURL string
	// SendURL overrides the send endpoint, for tests.
	SendURL string
}

// FCM is a Firebase Cloud Messaging v1 transport.
type FCM struct {
	account  ServiceAccount
	key      *rsa.PrivateKey
	client   *http.Client
	tokenURL string
	sendURL  string

	mu       sync.Mutex
	token    string
	tokenAge time.Time
}

// NewFCM builds a transport from a service-account key.
func NewFCM(cfg FCMConfig) (*FCM, error) {
	var acct ServiceAccount
	if err := json.Unmarshal(cfg.ServiceAccountJSON, &acct); err != nil {
		return nil, fmt.Errorf("fcm: service account key is not valid JSON (is this the file downloaded from Firebase console > Project settings > Service accounts?): %w", err)
	}
	if acct.ProjectID == "" || acct.ClientEmail == "" || acct.PrivateKey == "" {
		return nil, fmt.Errorf("fcm: service account key is missing project_id, client_email or private_key")
	}

	key, err := parseRSAKey([]byte(acct.PrivateKey))
	if err != nil {
		return nil, err
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		tokenURL = acct.TokenURI
	}
	if tokenURL == "" {
		tokenURL = fcmTokenURL
	}
	sendURL := cfg.SendURL
	if sendURL == "" {
		sendURL = fmt.Sprintf(fcmSendFormat, acct.ProjectID)
	}

	return &FCM{account: acct, key: key, client: client, tokenURL: tokenURL, sendURL: sendURL}, nil
}

// parseRSAKey decodes the PEM private key from a service account.
func parseRSAKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("fcm: private_key is not valid PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("fcm: parsing private_key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("fcm: private_key is %T, want RSA", parsed)
	}
	return key, nil
}

// accessToken returns a cached or freshly minted OAuth token.
func (f *FCM) accessToken(now time.Time) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token != "" && now.Sub(f.tokenAge) < fcmTokenLifetime {
		return f.token, nil
	}

	assertion, err := signRS256JWT(f.key, f.account.ClientEmail, fcmScope, f.tokenURL, now)
	if err != nil {
		return "", err
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fcm: exchanging the service-account key for a token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fcm: token exchange returned %d: %s",
			resp.StatusCode, readLimited(resp.Body, 1024))
	}

	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("fcm: token response was not JSON: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("fcm: token response contained no access_token")
	}

	f.token, f.tokenAge = out.AccessToken, now
	return out.AccessToken, nil
}

// signRS256JWT builds the service-account assertion Google expects.
func signRS256JWT(key *rsa.PrivateKey, issuer, scope, audience string, now time.Time) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iss":   issuer,
		"scope": scope,
		"aud":   audience,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
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
	sig, err := rsaSignPKCS1v15(key, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64(sig), nil
}

// BuildFCMPayload renders the v1 message body.
//
// Data-only: no "notification" key. A notification-payload message is rendered
// by the system before the app runs, so the sealed content would never be
// decrypted and the user would only ever see the minimal cloud text.
func BuildFCMPayload(n Notification, token, sealed string) ([]byte, error) {
	data := map[string]string{
		"title":      n.Alert.Title,
		"body":       n.Alert.Body,
		"session_id": n.SessionID,
		"kind":       string(n.Kind),
		"muxscale":   sealed,
	}
	if n.RequiresUnlock {
		data["requires_unlock"] = "1"
	}

	msg := map[string]any{
		"message": map[string]any{
			"token": token,
			"data":  data,
			"android": map[string]any{
				// high priority wakes the app promptly; the whole product is
				// "tell me now", so anything else defeats the purpose.
				"priority": "high",
				// Collapsing by session means a chatty session replaces rather
				// than burying everything else.
				"collapse_key": n.CollapseKey,
			},
		},
	}
	return json.Marshal(msg)
}

// Send delivers a notification to one device token.
func (f *FCM) Send(n Notification, tok Token) Result {
	body, err := BuildFCMPayload(n, tok.Token, n.Sealed[tok.DeviceID])
	if err != nil {
		return Result{Err: err}
	}

	access, err := f.accessToken(time.Now())
	if err != nil {
		return Result{Err: err}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.sendURL, bytes.NewReader(body))
	if err != nil {
		return Result{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		return Result{Err: fmt.Errorf("fcm request: %w", err)}
	}
	defer resp.Body.Close()

	return interpretFCMStatus(resp.StatusCode, readLimited(resp.Body, 4096))
}

// interpretFCMStatus maps a response onto a Result.
//
// UNREGISTERED and a 404 both mean the token is dead, the same as APNs 410.
// Everything transient is left retryable; everything else is a configuration
// error the operator needs to see.
func interpretFCMStatus(status int, body string) Result {
	switch {
	case status == http.StatusOK:
		return Result{OK: true}
	case status == http.StatusNotFound,
		strings.Contains(body, "UNREGISTERED"),
		strings.Contains(body, "registration-token-not-registered"):
		return Result{Unregistered: true, Err: fmt.Errorf("fcm: token is no longer registered")}
	case status == http.StatusBadRequest && strings.Contains(body, "INVALID_ARGUMENT") &&
		strings.Contains(body, "token"):
		return Result{Unregistered: true, Err: fmt.Errorf("fcm: token is invalid")}
	case status == http.StatusTooManyRequests:
		return Result{Err: fmt.Errorf("fcm 429: rate limited, retry later")}
	case status >= 500:
		return Result{Err: fmt.Errorf("fcm %d: transient server error", status)}
	default:
		return Result{Err: fmt.Errorf("fcm %d: %s", status, strings.TrimSpace(body))}
	}
}

var _ = base64.StdEncoding
