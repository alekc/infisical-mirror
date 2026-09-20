// Package infisical is a small client for the parts of the Infisical API a
// mirror needs: listing a folder tree, creating folders, and writing secrets in
// batches.
package infisical

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeout = 30 * time.Second
	maxAttempts    = 4
	baseBackoff    = 250 * time.Millisecond

	// Read bodies only far enough to quote an error. A secret list is large and
	// an error is not, so anything past this is a malfunctioning server.
	maxErrorBody = 4 << 10

	// Clamp the server's own message too. A message this long is not a message,
	// and the shorter it is the less room there is for it to carry something
	// back that should not travel.
	maxErrorMessage = 512

	// Cap on a Retry-After the server asks for. Honouring an hour-long hint
	// would hang a CronJob well past its next run.
	maxRetryAfter = 30 * time.Second
)

// Client talks to one Infisical instance.
type Client struct {
	baseURL string
	http    *http.Client
	tokens  TokenSource
	now     func() time.Time

	// sleep is nil in production, where waiting happens on a timer that a
	// cancelled context can interrupt. A test injects one to skip the wait and
	// to observe the schedule.
	sleep func(time.Duration)

	// Pointers, not values: withoutAuth clones the Client and both caches hold
	// a mutex, which must not be copied.
	folders  *folderCache
	projects *projectCache
}

// TokenSource yields a bearer token, refreshing it when needed.
type TokenSource interface {
	Token(ctx context.Context, c *Client) (string, error)
}

// Option customises a Client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client, for tests and for callers that need
// their own transport.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithClock replaces the clock and the sleep used between retries. Tests use it
// to exercise token expiry and backoff without waiting, and to assert the
// schedule: the delay a retry would have waited is passed to sleep, so a test
// that records the durations can tell a correct backoff from no backoff at all.
func WithClock(now func() time.Time, sleep func(time.Duration)) Option {
	return func(c *Client) {
		c.now = now
		c.sleep = sleep
	}
}

// isLoopback reports whether host is the local machine, the one place where
// plaintext http costs nothing because the traffic never reaches a wire.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// New builds a client for an instance. The URL is the instance root, with or
// without a trailing slash: the /api prefix belongs to this package.
func New(instanceURL string, tokens TokenSource, opts ...Option) (*Client, error) {
	if tokens == nil {
		return nil, errors.New("infisical: a token source is required")
	}

	u, err := url.Parse(strings.TrimRight(instanceURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("infisical: instance url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("infisical: instance url %q must be an absolute http or https url", instanceURL)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("infisical: instance url %q is plaintext http: every request carries a bearer token "+
			"and every reply carries secret values, so only loopback may be plaintext", instanceURL)
	}

	c := &Client{
		baseURL: u.String(),
		http: &http.Client{
			Timeout: defaultTimeout,
			// Go strips the Authorization header across a redirect only when the
			// hostname changes, never on the scheme, so an https URL redirecting
			// to http on the same host re-sends the token in the clear. An API
			// has no business redirecting us anywhere, so none are followed.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		tokens:   tokens,
		now:      time.Now,
		folders:  &folderCache{},
		projects: &projectCache{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// String keeps the token source, and so the credential inside it, out of
// anything that prints a Client.
func (c *Client) String() string {
	return fmt.Sprintf("infisical.Client{baseURL: %q}", c.baseURL)
}

// GoString keeps the credential out of %#v, which ignores String.
func (c *Client) GoString() string { return c.String() }

// noAuth is the token source used for the login call itself, which is the one
// request that must not carry a bearer token.
type noAuth struct{}

func (noAuth) Token(context.Context, *Client) (string, error) { return "", nil }

// withoutAuth returns a shallow copy that sends no Authorization header,
// sharing the underlying HTTP client and folder cache.
func (c *Client) withoutAuth() *Client {
	clone := *c
	clone.tokens = noAuth{}
	return &clone
}

// APIError is a non-2xx response.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	Message    string

	// RetryAfter is the server's own hint, when it sent a usable one.
	RetryAfter time.Duration
}

// AuthError is a failure to obtain a credential, as opposed to a failure of the
// request that wanted one. It deliberately does not implement Unwrap, so an
// auth 404 or 409 is not read as one from the API call, and it is terminal for
// the retry loop so retries do not nest. See docs/design.md.
type AuthError struct {
	Message string
	Err     error
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("infisical: %s: %v", e.Message, e.Err)
}

// terminalError marks a failure that repeating cannot fix: a request that could
// not be built, or a reply that could not be decoded. Without it each of those
// costs three extra round trips and the full backoff before it surfaces.
type terminalError struct{ err error }

func (e *terminalError) Error() string { return e.err.Error() }
func (e *terminalError) Unwrap() error { return e.err }

func terminal(err error) error { return &terminalError{err: err} }

// retryable reports whether repeating the call could plausibly change the
// answer, and returns the delay the server asked for when it named one.
func retryable(err error) (bool, time.Duration) {
	var authErr *AuthError
	if errors.As(err, &authErr) {
		return false, 0
	}
	var term *terminalError
	if errors.As(err, &term) {
		return false, 0
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable(), apiErr.RetryAfter
	}
	// A transport error: the connection failed, timed out, or was reset.
	return true, 0
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("infisical: %s %s: %d %s", e.Method, e.Path, e.StatusCode, msg)
}

// Retryable reports whether repeating the request could plausibly work. 4xx
// stays failed, apart from rate limiting.
func (e *APIError) Retryable() bool {
	return e.StatusCode >= 500 || e.StatusCode == http.StatusTooManyRequests
}

// IsNotFound reports whether err is a 404. A missing folder is the common one,
// and it is a create, not a failure.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// IsConflict reports whether err is a 409, which a concurrent create produces.
func IsConflict(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}

// do issues one authenticated request and decodes a JSON response into out,
// which may be nil when the body is not needed. Retries are bounded and apply
// only to failures that repeating could fix. body is marshalled once and
// replayed per attempt, so a retry never sends a partially consumed reader.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return fmt.Errorf("infisical: encoding request for %s: %w", path, err)
		}
	}

	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var (
		lastErr error
		delay   time.Duration
	)
	for attempt := range maxAttempts {
		if attempt > 0 {
			if err := c.wait(ctx, delay); err != nil {
				return err
			}
		}

		err := c.attempt(ctx, method, path, endpoint, encoded, out)
		if err == nil {
			return nil
		}
		lastErr = err

		again, retryAfter := retryable(err)
		if !again {
			return err
		}
		if ctx.Err() != nil {
			return err
		}
		delay = max(baseBackoff<<attempt, retryAfter)
	}
	return lastErr
}

// wait pauses between attempts. In production it is interruptible: a SIGTERM
// during a plan should not have to wait out the rest of the backoff, which at
// the top of the schedule is most of two seconds per call. A test injects a
// sleep instead, which both skips the wait and records what it would have been.
func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.sleep != nil {
		c.sleep(d)
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) attempt(ctx context.Context, method, path, endpoint string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return terminal(fmt.Errorf("infisical: building request for %s: %w", path, err))
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// A token source that needs to log in does so through this same client, so
	// it is asked for a token per attempt rather than once per call.
	token, err := c.tokens.Token(ctx, c)
	if err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) {
			return err
		}
		return &AuthError{Message: "obtaining a credential", Err: err}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("infisical: %s %s: %w", method, path, err)
	}
	// Drain before closing, or the connection cannot be reused and every call
	// pays for a fresh TCP and TLS handshake.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
	}()

	// Redirects are not followed (see New), so a 3xx arrives here as itself.
	// Name the destination, because the likely cause is an instance URL that
	// points at a front end rather than at the API.
	if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		return &APIError{
			StatusCode: resp.StatusCode,
			Method:     method,
			Path:       path,
			Message:    fmt.Sprintf("redirected to %q, which this client does not follow", resp.Header.Get("Location")),
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{
			StatusCode: resp.StatusCode,
			Method:     method,
			Path:       path,
			Message:    errorMessage(resp.Body, path),
			RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
		}
	}

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return terminal(fmt.Errorf("infisical: decoding %s %s: %w", method, path, err))
	}
	return nil
}

// carriesSecrets reports whether a request to this path has credentials or
// secret values in its body.
func carriesSecrets(path string) bool {
	return strings.HasPrefix(path, "/api/v1/auth/") || strings.HasPrefix(path, "/api/v4/secrets/batch")
}

// errorMessage pulls the server's own message out of an error body and never
// quotes an unrecognised one. A proxy that echoes the payload it rejected hands
// the request back, and on the login endpoint that request body is the client
// secret. See docs/design.md.
func errorMessage(r io.Reader, path string) string {
	raw, err := io.ReadAll(io.LimitReader(r, maxErrorBody))
	if err != nil || len(raw) == 0 {
		return ""
	}
	if carriesSecrets(path) {
		return ""
	}

	var parsed struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil {
		if msg := clamp(parsed.Message); msg != "" {
			return msg
		}
		if msg := clamp(parsed.Error); msg != "" {
			return msg
		}
	}
	return fmt.Sprintf("(unparseable %d-byte body)", len(raw))
}

func clamp(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxErrorMessage {
		return s[:maxErrorMessage] + "..."
	}
	return s
}

// retryAfter reads the header of that name, accepting the delay-seconds form
// and ignoring the HTTP-date form, which no Infisical response uses.
func retryAfter(header string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || secs <= 0 {
		return 0
	}
	return min(time.Duration(secs)*time.Second, maxRetryAfter)
}
