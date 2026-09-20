package infisical

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// renewSkew renews a machine-identity token this far before it expires, so a
// long request cannot start on a token that dies mid-flight.
const renewSkew = 60 * time.Second

// StaticToken is a token supplied whole, typically a service token read from
// the environment. It never expires as far as this process is concerned: when
// it does expire the API says so, and the answer is a new token, not a retry.
type StaticToken struct {
	token string
}

// NewStaticToken builds a static token source.
func NewStaticToken(token string) (*StaticToken, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("infisical: token is empty")
	}
	return &StaticToken{token: token}, nil
}

// Token implements TokenSource.
func (s *StaticToken) Token(context.Context, *Client) (string, error) { return s.token, nil }

// String keeps the token out of anything that prints this value. An unexported
// field is not redaction: fmt reaches one by reflection for %+v, and %#v does
// so even past a String method, so only the pair of String and GoString covers
// every verb. See docs/design.md.
func (s *StaticToken) String() string   { return "infisical.StaticToken{token: <redacted>}" }
func (s *StaticToken) GoString() string { return s.String() }

// UniversalAuth logs in with a machine identity and caches the access token
// until shortly before it expires.
type UniversalAuth struct {
	clientID     string
	clientSecret string

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewUniversalAuth builds a machine-identity token source.
func NewUniversalAuth(clientID, clientSecret string) (*UniversalAuth, error) {
	if strings.TrimSpace(clientID) == "" {
		return nil, errors.New("infisical: universal auth client id is empty")
	}
	if strings.TrimSpace(clientSecret) == "" {
		return nil, errors.New("infisical: universal auth client secret is empty")
	}
	return &UniversalAuth{clientID: clientID, clientSecret: clientSecret}, nil
}

// String keeps the client secret out of anything that prints this value. See
// StaticToken.String for why the unexported fields are not enough on their own.
func (u *UniversalAuth) String() string {
	return "infisical.UniversalAuth{clientID: <redacted>, clientSecret: <redacted>}"
}

// GoString keeps the client secret out of %#v, which ignores String.
func (u *UniversalAuth) GoString() string { return u.String() }

// Token implements TokenSource, logging in on first use and on expiry.
func (u *UniversalAuth) Token(ctx context.Context, c *Client) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.token != "" && c.now().Before(u.expires.Add(-renewSkew)) {
		return u.token, nil
	}

	body := map[string]string{
		"clientId":     u.clientID,
		"clientSecret": u.clientSecret,
	}
	var resp struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int64  `json:"expiresIn"`
	}
	// The login is the one unauthenticated call, so it goes through a clone with
	// no bearer token rather than recursing. Every failure leaves as an
	// AuthError, which does not unwrap, so a 404 here is not read as a missing
	// project and the caller's retries do not nest. See docs/design.md.
	if err := c.withoutAuth().do(ctx, "POST", "/api/v1/auth/universal-auth/login", nil, body, &resp); err != nil {
		return "", &AuthError{Message: "universal auth login", Err: err}
	}
	if resp.AccessToken == "" {
		return "", &AuthError{Message: "universal auth login", Err: errors.New("no access token in the response")}
	}
	// A token already inside the renewal skew can never satisfy the cache check
	// above, so every request would log in again: a silent storm against the
	// endpoint least able to absorb one. The likely causes are a moved field and
	// a unit that is not seconds, both worth stopping for.
	lifetime := time.Duration(resp.ExpiresIn) * time.Second
	if lifetime <= renewSkew {
		return "", &AuthError{
			Message: "universal auth login",
			Err: fmt.Errorf("the access token expires in %v, at or inside the %v renewal margin, so it can never be cached",
				lifetime, renewSkew),
		}
	}

	u.token = resp.AccessToken
	u.expires = c.now().Add(lifetime)
	return u.token, nil
}

// TokenSourceFromEnv builds a token source from the environment variable names
// a config names. Exactly one of the two forms must be supplied; the values
// themselves never appear in the config, and never in an error here either.
func TokenSourceFromEnv(clientIDEnv, clientSecretEnv, tokenEnv string) (TokenSource, error) {
	switch {
	case tokenEnv != "":
		token := os.Getenv(tokenEnv)
		if strings.TrimSpace(token) == "" {
			return nil, fmt.Errorf("infisical: environment variable %s is unset or empty", tokenEnv)
		}
		return NewStaticToken(token)

	case clientIDEnv != "" && clientSecretEnv != "":
		id, secret := os.Getenv(clientIDEnv), os.Getenv(clientSecretEnv)
		var missing []string
		if strings.TrimSpace(id) == "" {
			missing = append(missing, clientIDEnv)
		}
		if strings.TrimSpace(secret) == "" {
			missing = append(missing, clientSecretEnv)
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("infisical: environment variable(s) unset or empty: %s", strings.Join(missing, ", "))
		}
		return NewUniversalAuth(id, secret)

	default:
		return nil, errors.New("infisical: no credential environment variables named")
	}
}
