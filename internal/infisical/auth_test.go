package infisical

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The login call is the one request that must not carry a bearer token, and its
// result must be reused rather than re-fetched per call.
func TestUniversalAuthLogsInOnceAndCaches(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.URL.Path == "/api/v1/auth/universal-auth/login" {
			writeJSON(t, w, 200, map[string]any{"accessToken": "fresh-token", "expiresIn": 3600})
			return
		}
		writeJSON(t, w, 200, map[string]any{"secrets": []any{}})
	})

	ua, err := NewUniversalAuth("id-1", "secret-1")
	if err != nil {
		t.Fatalf("NewUniversalAuth: %v", err)
	}
	now := time.Now()
	c, err := New(fake.URL, ua, WithClock(func() time.Time { return now }, func(time.Duration) {}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	for range 3 {
		if _, err := c.ListSecrets(ctx, ListRequest{ProjectID: "p", Environment: "prod", Path: "/"}); err != nil {
			t.Fatalf("ListSecrets: %v", err)
		}
	}

	got := fake.seen()
	if len(got) != 4 {
		t.Fatalf("want 1 login plus 3 listings, got %d requests", len(got))
	}
	if got[0].Path != "/api/v1/auth/universal-auth/login" {
		t.Fatalf("first request was %s, want the login", got[0].Path)
	}
	if got[0].Auth != "" {
		t.Errorf("the login carried an Authorization header: %q", got[0].Auth)
	}
	if got[0].Body["clientId"] != "id-1" || got[0].Body["clientSecret"] != "secret-1" {
		t.Errorf("login body = %v", got[0].Body)
	}
	for _, req := range got[1:] {
		if req.Auth != "Bearer fresh-token" {
			t.Errorf("%s carried %q", req.Path, req.Auth)
		}
	}
}

// The token is renewed shortly before it expires, not after, so a request
// cannot start on a token that dies in flight.
func TestUniversalAuthRenewsBeforeExpiry(t *testing.T) {
	logins := 0
	fake := newFakeServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.URL.Path == "/api/v1/auth/universal-auth/login" {
			logins++
			writeJSON(t, w, 200, map[string]any{
				"accessToken": "token-" + string(rune('0'+logins)),
				"expiresIn":   600,
			})
			return
		}
		writeJSON(t, w, 200, map[string]any{"secrets": []any{}})
	})

	ua, err := NewUniversalAuth("id", "secret")
	if err != nil {
		t.Fatalf("NewUniversalAuth: %v", err)
	}
	start := time.Now()
	now := start
	c, err := New(fake.URL, ua, WithClock(func() time.Time { return now }, func(time.Duration) {}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	list := func() {
		t.Helper()
		if _, err := c.ListSecrets(ctx, ListRequest{ProjectID: "p", Environment: "prod", Path: "/"}); err != nil {
			t.Fatalf("ListSecrets: %v", err)
		}
	}

	list()
	// Still inside the window, minus the renewal skew.
	now = start.Add(500 * time.Second)
	list()
	if logins != 1 {
		t.Fatalf("logins = %d before expiry, want 1", logins)
	}

	// Inside the skew: the token has not expired yet, and is renewed anyway.
	now = start.Add(600*time.Second - renewSkew/2)
	list()
	if logins != 2 {
		t.Fatalf("logins = %d inside the renewal skew, want 2", logins)
	}
}

func TestUniversalAuthLoginFailures(t *testing.T) {
	tests := map[string]func(w http.ResponseWriter){
		"bad credentials": func(w http.ResponseWriter) {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"message":"invalid credentials"}`)
		},
		"empty token": func(w http.ResponseWriter) {
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"accessToken":"","expiresIn":3600}`)
		},
	}

	for name, respond := range tests {
		t.Run(name, func(t *testing.T) {
			fake := newFakeServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				if r.URL.Path == "/api/v1/auth/universal-auth/login" {
					respond(w)
					return
				}
				t.Error("no API call should follow a failed login")
				writeJSON(t, w, 200, map[string]any{})
			})

			ua, err := NewUniversalAuth("id", "secret")
			if err != nil {
				t.Fatalf("NewUniversalAuth: %v", err)
			}
			c, err := New(fake.URL, ua, WithClock(time.Now, func(time.Duration) {}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = c.ListSecrets(context.Background(), ListRequest{ProjectID: "p", Environment: "prod", Path: "/"})
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), "secret") && strings.Contains(err.Error(), "clientSecret") {
				t.Errorf("error leaked credentials: %v", err)
			}
		})
	}
}

func TestTokenSourceFromEnv(t *testing.T) {
	t.Setenv("MIRROR_TEST_TOKEN", "  service-token  ")
	t.Setenv("MIRROR_TEST_ID", "client-id")
	t.Setenv("MIRROR_TEST_SECRET", "client-secret")
	t.Setenv("MIRROR_TEST_BLANK", "   ")

	t.Run("static token is trimmed", func(t *testing.T) {
		src, err := TokenSourceFromEnv("", "", "MIRROR_TEST_TOKEN")
		if err != nil {
			t.Fatalf("TokenSourceFromEnv: %v", err)
		}
		got, err := src.Token(context.Background(), nil)
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if got != "service-token" {
			t.Errorf("token = %q", got)
		}
	})

	t.Run("universal auth", func(t *testing.T) {
		src, err := TokenSourceFromEnv("MIRROR_TEST_ID", "MIRROR_TEST_SECRET", "")
		if err != nil {
			t.Fatalf("TokenSourceFromEnv: %v", err)
		}
		if _, ok := src.(*UniversalAuth); !ok {
			t.Fatalf("want a *UniversalAuth, got %T", src)
		}
	})

	// A variable that is unset, or set to whitespace, fails at construction.
	// Authenticating with an empty credential would otherwise fail later and
	// look like a permission problem on the instance.
	rejects := map[string][3]string{
		"unset token":          {"", "", "MIRROR_TEST_MISSING"},
		"blank token":          {"", "", "MIRROR_TEST_BLANK"},
		"missing client id":    {"MIRROR_TEST_MISSING", "MIRROR_TEST_SECRET", ""},
		"blank client secret":  {"MIRROR_TEST_ID", "MIRROR_TEST_BLANK", ""},
		"nothing named at all": {"", "", ""},
		"only a client id":     {"MIRROR_TEST_ID", "", ""},
	}
	for name, env := range rejects {
		t.Run(name, func(t *testing.T) {
			if _, err := TokenSourceFromEnv(env[0], env[1], env[2]); err == nil {
				t.Fatalf("want an error for %v", env)
			}
		})
	}

	// The error names the variable, never its contents.
	_, err := TokenSourceFromEnv("", "", "MIRROR_TEST_BLANK")
	if err == nil || !strings.Contains(err.Error(), "MIRROR_TEST_BLANK") {
		t.Fatalf("error should name the variable, got: %v", err)
	}
}

// The login is an ordinary request through this same client, so without a
// barrier in AuthError its status leaks into the callers' classification: a 404
// from a mistyped URL would satisfy IsNotFound, a 409 would satisfy IsConflict.
// AuthError does not implement Unwrap, and this is what that is for.
func TestAuthErrorDoesNotLeakItsStatusToCallers(t *testing.T) {
	for _, status := range []int{401, 403, 404, 409, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var logins int
			fake := newFakeServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				if r.URL.Path == "/api/v1/auth/universal-auth/login" {
					logins++
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"message":"nope"}`)
					return
				}
				t.Error("no API call should follow a failed login")
				writeJSON(t, w, 200, map[string]any{})
			})

			ua, err := NewUniversalAuth("id", "secret")
			if err != nil {
				t.Fatalf("NewUniversalAuth: %v", err)
			}
			c, err := New(fake.URL, ua, WithClock(time.Now, func(time.Duration) {}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = c.ListSecrets(context.Background(), ListRequest{ProjectID: "p", Environment: "prod", Path: "/"})
			if err == nil {
				t.Fatal("want an error")
			}

			var authErr *AuthError
			if !errors.As(err, &authErr) {
				t.Fatalf("want an *AuthError, got %T: %v", err, err)
			}
			if IsNotFound(err) {
				t.Errorf("a %d login failure was reported as a missing resource: %v", status, err)
			}
			if IsConflict(err) {
				t.Errorf("a %d login failure was reported as a conflict: %v", status, err)
			}
			var apiErr *APIError
			if errors.As(err, &apiErr) {
				t.Errorf("the login's APIError reached the caller: %v", apiErr)
			}

			// An AuthError is terminal for the outer retry loop as well. The
			// login does its own retrying, so retrying around it multiplies the
			// schedules: maxAttempts squared calls for one failed credential.
			if logins > maxAttempts {
				t.Errorf("the server saw %d logins, want at most %d: the retry loops are nested", logins, maxAttempts)
			}
		})
	}
}
