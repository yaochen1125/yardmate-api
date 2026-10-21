package referral

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DeviceCheck — anti-abuse gate 1 (invite-friends.md ### 防刷 第 1 道): one
// device may be a referred friend once in its life. Apple keeps two bits per
// device per developer team; bit0 = "this device has been a referred friend",
// bit1 is left alone. The bits survive a reinstall and are invisible to the
// user; the production and development environments keep separate bits.
//
//	/claim (new claim only), all in ONE database transaction:
//	    global device lock (ClaimGate) → steps 2–4 again
//	    → query_two_bits: bit0 set → device_used (rollback)
//	    → referral_claim (a pool code) → update_two_bits(bit0) → commit
//	  the set answers an error → read bit0 back: set → keep the claim;
//	  clear / unreadable → rollback, the code stays free → device_check_unavailable
//
// The lock is what makes check-then-set atomic: Apple's bits are not in our
// transaction, and one device can come in as many accounts at once, each
// with its own (forged) install id. A re-claim (same account, same code)
// never calls Apple.

// DeviceCheckMode is DEVICECHECK_MODE.
type DeviceCheckMode string

const (
	// DeviceCheckEnforce (prod): no token / a token Apple rejects →
	// device_unverified; bit0 already set → device_used; Apple unreachable or
	// the set failing → device_check_unavailable. Nothing is let through.
	DeviceCheckEnforce DeviceCheckMode = "enforce"
	// DeviceCheckLog (staging): query and set exactly the same, but every
	// verdict is only logged — the same test phone can claim again and again.
	// Switch staging to enforce to test gate 1 itself.
	DeviceCheckLog DeviceCheckMode = "log"
)

// ParseDeviceCheckMode reads DEVICECHECK_MODE. Unset means enforce (fail
// closed: forgetting the setting must not open the gate); a value that is
// neither is also enforce, with ok=false so the caller can warn.
func ParseDeviceCheckMode(s string) (mode DeviceCheckMode, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(DeviceCheckEnforce):
		return DeviceCheckEnforce, true
	case string(DeviceCheckLog):
		return DeviceCheckLog, true
	}
	return DeviceCheckEnforce, false
}

// device_check_env values (claim body).
//
// enforce only accepts "production": a token from any other environment —
// "development", the field missing, anything else — is treated as no token
// (device_unverified) and Apple's development host is never asked. Its bits
// are kept apart from production's, so honouring it would give every device
// a second life (decided 2026-10-20 on api#126; the app's Release builds
// always send "production"). The one exception is
// DEVICECHECK_ALLOW_DEVELOPMENT=true (staging only, never prod): enforce then
// also accepts "development" and asks the development host, so gate 1 can be
// tested in enforce with a DEBUG build.
//
// log asks the environment the request names: "development" → the
// development host, anything else (missing included) → production.
const (
	DeviceEnvProduction  = "production"
	DeviceEnvDevelopment = "development"
)

// deviceEnv is the host log mode asks for a device_check_env value.
func deviceEnv(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), DeviceEnvDevelopment) {
		return DeviceEnvDevelopment
	}
	return DeviceEnvProduction
}

// isProductionEnv is enforce's test: the request explicitly says
// "production" (case and surrounding space ignored).
func isProductionEnv(s string) bool {
	return strings.EqualFold(strings.TrimSpace(s), DeviceEnvProduction)
}

// isDevelopmentEnv: the request explicitly says "development" (what enforce
// also accepts with DEVICECHECK_ALLOW_DEVELOPMENT).
func isDevelopmentEnv(s string) bool {
	return strings.EqualFold(strings.TrimSpace(s), DeviceEnvDevelopment)
}

// ErrDeviceTokenInvalid is Apple refusing the device token itself (HTTP 400
// "Missing or incorrectly formatted device token payload"). Every other
// DeviceChecker error means Apple was not usable this time.
var ErrDeviceTokenInvalid = errors.New("devicecheck: device token rejected")

// DeviceChecker is the slice of Apple's DeviceCheck server API gate 1 uses.
// An interface so the handler tests run against a fake.
type DeviceChecker interface {
	// QueryBit0 reports whether bit0 is set for the device behind token in
	// env (DeviceEnvProduction / DeviceEnvDevelopment). A device Apple holds
	// no bits for is false, not an error.
	QueryBit0(ctx context.Context, env, token string) (bool, error)
	// SetBit0 sets bit0 = true for the device; bit1 is not sent (unchanged).
	SetBit0(ctx context.Context, env, token string) error
}

const (
	appleDeviceCheckProd = "https://api.devicecheck.apple.com"
	appleDeviceCheckDev  = "https://api.development.devicecheck.apple.com"
	// DefaultDeviceCheckTimeout bounds each Apple call, so /claim cannot hang
	// on Apple (docs: 超时 → device_check_unavailable).
	DefaultDeviceCheckTimeout = 3 * time.Second
	deviceCheckMaxBody        = 16 << 10
	deviceCheckUserAgent      = "yardmate-api referral"
)

// AppleDeviceCheck calls Apple's DeviceCheck server API with an ES256 JWT
// (kid = DEVICECHECK_KEY_ID, iss = APPLE_TEAM_ID, iat = now) signed by the
// DeviceCheck .p8 key. The key lives only in this struct and is never logged;
// neither is a device token.
type AppleDeviceCheck struct {
	keyID, teamID string
	key           *ecdsa.PrivateKey
	http          *http.Client
	prodBase      string
	devBase       string
	now           func() time.Time
}

// NewAppleDeviceCheck parses the .p8 (PKCS#8 or SEC1 PEM) and returns a
// client. The error never contains the key material.
func NewAppleDeviceCheck(keyID, teamID string, p8 []byte) (*AppleDeviceCheck, error) {
	keyID, teamID = strings.TrimSpace(keyID), strings.TrimSpace(teamID)
	if keyID == "" || teamID == "" {
		return nil, errors.New("devicecheck: key id and team id are required")
	}
	key, err := jwt.ParseECPrivateKeyFromPEM(p8)
	if err != nil {
		return nil, errors.New("devicecheck: private key is not an EC .p8 PEM")
	}
	return &AppleDeviceCheck{
		keyID: keyID, teamID: teamID, key: key,
		http:     &http.Client{Timeout: DefaultDeviceCheckTimeout},
		prodBase: appleDeviceCheckProd,
		devBase:  appleDeviceCheckDev,
		now:      time.Now,
	}, nil
}

// SetBases overrides both API hosts (tests).
func (c *AppleDeviceCheck) SetBases(prod, dev string) {
	c.prodBase, c.devBase = strings.TrimRight(prod, "/"), strings.TrimRight(dev, "/")
}

// SetTimeout overrides the per-call HTTP timeout (tests).
func (c *AppleDeviceCheck) SetTimeout(d time.Duration) { c.http.Timeout = d }

// QueryBit0 is POST /v1/query_two_bits. 200 with {"bit0":…} is the answer;
// 200 with Apple's plain-text "Failed to find bit state" / "Bit State Not
// Found" means the device has never had bits set — never a referred friend.
func (c *AppleDeviceCheck) QueryBit0(ctx context.Context, env, token string) (bool, error) {
	raw, status, err := c.post(ctx, env, "/v1/query_two_bits", token, nil)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusBadRequest:
		return false, ErrDeviceTokenInvalid
	default:
		return false, fmt.Errorf("devicecheck: query_two_bits status %d", status)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var bits struct {
			Bit0 bool `json:"bit0"`
		}
		if err := json.Unmarshal(trimmed, &bits); err != nil {
			return false, fmt.Errorf("devicecheck: decode query_two_bits: %w", err)
		}
		return bits.Bit0, nil
	}
	if strings.Contains(strings.ToLower(string(trimmed)), "bit state") {
		return false, nil
	}
	return false, errors.New("devicecheck: query_two_bits: unexpected 200 body")
}

// SetBit0 is POST /v1/update_two_bits {"bit0": true}.
func (c *AppleDeviceCheck) SetBit0(ctx context.Context, env, token string) error {
	_, status, err := c.post(ctx, env, "/v1/update_two_bits", token, map[string]any{"bit0": true})
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusBadRequest:
		return ErrDeviceTokenInvalid
	}
	return fmt.Errorf("devicecheck: update_two_bits status %d", status)
}

// post sends one request: device_token + transaction_id + timestamp (ms),
// plus extra. Only the status and body come back; errors carry neither the
// token nor the JWT.
func (c *AppleDeviceCheck) post(ctx context.Context, env, path, token string, extra map[string]any) ([]byte, int, error) {
	if c == nil {
		return nil, 0, errors.New("devicecheck: no client")
	}
	bearer, err := c.bearer()
	if err != nil {
		return nil, 0, err
	}
	body := map[string]any{
		"device_token":   token,
		"transaction_id": newTransactionID(),
		"timestamp":      c.now().UnixMilli(),
	}
	for k, v := range extra {
		body[k] = v
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	base := c.prodBase
	if env == DeviceEnvDevelopment {
		base = c.devBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", deviceCheckUserAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error names the URL, never the body or headers.
		return nil, 0, fmt.Errorf("devicecheck: %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, deviceCheckMaxBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("devicecheck: read %s: %w", path, err)
	}
	return raw, resp.StatusCode, nil
}

// bearer signs the request JWT. A fresh one per call: one ES256 signature
// costs microseconds, and there is no cache to go stale (Apple rejects an
// iat older than an hour).
func (c *AppleDeviceCheck) bearer() (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": c.teamID,
		"iat": c.now().Unix(),
	})
	tok.Header["kid"] = c.keyID
	signed, err := tok.SignedString(c.key)
	if err != nil {
		return "", fmt.Errorf("devicecheck: sign jwt: %w", err)
	}
	return signed, nil
}

// newTransactionID is the per-request id Apple asks for (any unique string):
// a random v4-shaped UUID.
func newTransactionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("yardmate-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
