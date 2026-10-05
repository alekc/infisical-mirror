// Package webhook receives Infisical's project webhooks and turns a verified
// secrets.modified event into a request for an early pass. The event never
// says which secret changed, so it is a trigger, not data. See docs/design.md.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Event types this package tells apart. Anything else is ignored.
const (
	EventSecretsModified = "secrets.modified"
	EventTest            = "test"
)

// MaxSkew is how far the signed timestamp may sit from the local clock. It
// bounds how long a captured request can be replayed.
const MaxSkew = 5 * time.Minute

var (
	ErrUnsigned     = errors.New("no x-infisical-signature header")
	ErrMalformed    = errors.New("malformed signature header or body")
	ErrBadSignature = errors.New("signature does not match")
	ErrStale        = errors.New("timestamp outside the allowed skew")
)

// Event is the part of a verified payload the daemon reads. Infisical also
// sends changedBy, which can be a person's email, so it is never decoded.
type Event struct {
	Type        string
	ProjectID   string
	Environment string
	SecretPath  string
}

// payload is the general webhook body, built by getWebhookPayload in
// Infisical's backend/src/services/webhook/webhook-fns.ts.
type payload struct {
	Event   string `json:"event"`
	Project struct {
		ProjectID   string `json:"projectId"`
		Environment string `json:"environment"`
		SecretPath  string `json:"secretPath"`
	} `json:"project"`
	Timestamp *int64 `json:"timestamp"`
}

// Verify checks a request the way Infisical signs it: the header is
// "t=<unix ms>;<hex>", the hex is HMAC-SHA256 of the raw body, and the same
// timestamp is a field of that body. The body is never re-encoded before
// hashing, since any re-encoding changes the bytes.
func Verify(secret []byte, header string, body []byte, now time.Time) (Event, error) {
	if header == "" {
		return Event{}, ErrUnsigned
	}
	ts, sig, err := parseHeader(header)
	if err != nil {
		return Event{}, err
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return Event{}, ErrBadSignature
	}

	var p payload
	if err := json.Unmarshal(body, &p); err != nil || p.Event == "" || p.Timestamp == nil {
		return Event{}, ErrMalformed
	}
	// The header copy is unsigned; the body copy is what the signature covers.
	if *p.Timestamp != ts {
		return Event{}, ErrMalformed
	}
	if skew := now.Sub(time.UnixMilli(*p.Timestamp)); skew > MaxSkew || skew < -MaxSkew {
		return Event{}, ErrStale
	}

	return Event{
		Type:        p.Event,
		ProjectID:   p.Project.ProjectID,
		Environment: p.Project.Environment,
		SecretPath:  p.Project.SecretPath,
	}, nil
}

// parseHeader splits "t=<digits>;<64 lowercase hex>" and accepts nothing
// looser, because Infisical produces exactly that and a lenient parser is
// where a second, unintended format starts being accepted.
func parseHeader(h string) (int64, []byte, error) {
	stamp, sig, ok := strings.Cut(h, ";")
	if !ok {
		return 0, nil, ErrMalformed
	}
	digits, ok := strings.CutPrefix(stamp, "t=")
	if !ok || digits == "" || strings.TrimLeft(digits, "0123456789") != "" {
		return 0, nil, ErrMalformed
	}
	ts, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, nil, ErrMalformed
	}
	if len(sig) != hex.EncodedLen(sha256.Size) || strings.ToLower(sig) != sig {
		return 0, nil, ErrMalformed
	}
	raw, err := hex.DecodeString(sig)
	if err != nil {
		return 0, nil, ErrMalformed
	}
	return ts, raw, nil
}
