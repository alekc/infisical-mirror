package infisical

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A bearer token and a reply full of secret values have no business on a
// plaintext connection, and the failure is silent: it works, so nothing ever
// reports it. Loopback is the exception, because that traffic never reaches a
// wire.
func TestNewRejectsPlaintextHTTP(t *testing.T) {
	tok, err := NewStaticToken("t")
	if err != nil {
		t.Fatalf("NewStaticToken: %v", err)
	}

	for _, url := range []string{
		"http://app.infisical.com",
		"http://secrets.internal.example",
		"http://10.0.0.5:8080",
	} {
		if _, err := New(url, tok); err == nil {
			t.Errorf("plaintext %q was accepted", url)
		}
	}

	for _, url := range []string{
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
		"https://app.infisical.com",
	} {
		if _, err := New(url, tok); err != nil {
			t.Errorf("New(%q): %v", url, err)
		}
	}
}

// Go's http.Client strips the Authorization header across a redirect only when
// the hostname changes. It never looks at the scheme, so https redirecting to
// http on the same host re-sends the bearer token, and carries the reply back,
// in the clear. Nothing in this API should redirect, so none are followed.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var downstream int
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstream++
		writeJSON(t, w, 200, map[string]any{"secrets": []any{}})
	}))
	t.Cleanup(sink.Close)

	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Location", sink.URL+"/api/v4/secrets")
		w.WriteHeader(http.StatusFound)
	})

	_, err := newTestClient(t, fake).ListSecrets(context.Background(), ListRequest{
		ProjectID: "p", Environment: "prod", Path: "/",
	})
	if err == nil {
		t.Fatal("want an error for a redirect")
	}
	if downstream != 0 {
		t.Errorf("the redirect was followed %d time(s), which re-sends the token", downstream)
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusFound {
		t.Fatalf("want a 302 APIError, got: %v", err)
	}
	// A 3xx is not retryable: repeating it gets the same redirect.
	if n := len(fake.seen()); n != 1 {
		t.Errorf("server saw %d calls, want 1", n)
	}
	// The likely cause is an instance URL pointing at a front end rather than
	// at the API, so the error has to name where it was being sent.
	if !strings.Contains(err.Error(), sink.URL) {
		t.Errorf("error should name the redirect destination, got: %v", err)
	}
}

// The schedule, not just the count. A retry loop that waits nothing hammers a
// rate-limited server four times in a millisecond and reports the same failure
// as one that backed off properly.
func TestBackoffSchedule(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(503)
	})

	var waits []time.Duration
	tok, err := NewStaticToken("t")
	if err != nil {
		t.Fatalf("NewStaticToken: %v", err)
	}
	c, err := New(fake.URL, tok, WithClock(time.Now, func(d time.Duration) {
		waits = append(waits, d)
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.ListSecrets(context.Background(), ListRequest{
		ProjectID: "p", Environment: "prod", Path: "/",
	}); err == nil {
		t.Fatal("want an error")
	}

	want := []time.Duration{baseBackoff, 2 * baseBackoff, 4 * baseBackoff}
	if len(waits) != len(want) {
		t.Fatalf("waited %d time(s) %v, want %d", len(waits), waits, len(want))
	}
	for i, d := range want {
		if waits[i] != d {
			t.Errorf("wait %d = %v, want %v", i+1, waits[i], d)
		}
	}
}

// Retry-After wins when the server asks for longer than the schedule, and the
// schedule wins when it asks for less: the point of the hint is to slow down,
// never to speed up.
func TestRetryAfterIsHonouredAndClamped(t *testing.T) {
	tests := map[string]struct {
		header string
		want   time.Duration
	}{
		"longer than the schedule": {"5", 5 * time.Second},
		"shorter is ignored":       {"0", baseBackoff},
		"an hour is clamped":       {"3600", maxRetryAfter},
		"garbage is ignored":       {"soon", baseBackoff},
		"absent":                   {"", baseBackoff},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(429)
			})

			var waits []time.Duration
			tok, _ := NewStaticToken("t")
			c, err := New(fake.URL, tok, WithClock(time.Now, func(d time.Duration) {
				waits = append(waits, d)
			}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := c.ListSecrets(context.Background(), ListRequest{
				ProjectID: "p", Environment: "prod", Path: "/",
			}); err == nil {
				t.Fatal("want an error")
			}
			if len(waits) == 0 {
				t.Fatal("no retry was attempted")
			}
			if waits[0] != tc.want {
				t.Errorf("first wait = %v, want %v", waits[0], tc.want)
			}
		})
	}
}

// A cancelled context stops the run rather than waiting out the rest of the
// backoff, which at the top of the schedule is most of two seconds per call.
// The production path has no injected sleep, so this exercises the timer.
func TestCancelledContextStopsTheRetryLoop(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(503)
	})

	tok, err := NewStaticToken("t")
	if err != nil {
		t.Fatalf("NewStaticToken: %v", err)
	}
	// No WithClock: the real interruptible wait is the thing under test.
	c, err := New(fake.URL, tok)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.ListSecrets(ctx, ListRequest{ProjectID: "p", Environment: "prod", Path: "/"})
		done <- err
	}()

	// Let the first attempt fail and the first backoff start, then cancel
	// inside it. The first wait is baseBackoff, so this lands mid-wait.
	time.Sleep(baseBackoff / 2)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want a cancelled context, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the call did not return: cancellation did not interrupt the backoff")
	}

	// It gave up mid-schedule rather than running to exhaustion.
	if n := len(fake.seen()); n >= maxAttempts {
		t.Errorf("server saw %d calls, want fewer than the %d a full run makes", n, maxAttempts)
	}
}

// Every response body is drained before it is closed, or the connection cannot
// be reused and each call pays for a fresh TCP and TLS handshake. Counting
// connections is the only way to see this: the calls succeed either way.
func TestBodiesAreDrainedSoConnectionsAreReused(t *testing.T) {
	var (
		mu    sync.Mutex
		conns = map[string]int{}
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns[r.RemoteAddr]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// A body with content: an empty one is drained by doing nothing.
		writeJSON(t, w, 200, map[string]any{"secrets": []map[string]any{
			{"secretKey": "A", "secretValue": "1", "type": "shared"},
			{"secretKey": "B", "secretValue": "2", "type": "shared"},
		}})
	}))
	t.Cleanup(srv.Close)

	tok, err := NewStaticToken("t")
	if err != nil {
		t.Fatalf("NewStaticToken: %v", err)
	}
	c, err := New(srv.URL, tok)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for range 5 {
		if _, err := c.ListSecrets(context.Background(), ListRequest{
			ProjectID: "p", Environment: "prod", Path: "/",
		}); err != nil {
			t.Fatalf("ListSecrets: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(conns) != 1 {
		t.Errorf("5 sequential calls opened %d connections, want 1: a body was left undrained", len(conns))
	}
}

// A plan reads both instances at once, so one client is used from several
// goroutines. The caches are the sharp edge; -race is what makes this test
// mean anything.
func TestConcurrentCallsShareOneClient(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/slug/"):
			writeJSON(t, w, 200, projectBody())
		case r.URL.Path == "/api/v2/folders":
			writeJSON(t, w, 200, map[string]any{"folders": []any{}})
		default:
			writeJSON(t, w, 200, map[string]any{"secrets": []map[string]any{
				{"secretKey": "A", "secretValue": "1", "type": "shared"},
			}})
		}
	})
	c := newTestClient(t, fake)

	var wg sync.WaitGroup
	errs := make(chan error, 60)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			if _, err := c.ResolveProjectID(ctx, "home-cluster-k-wv9", "prod"); err != nil {
				errs <- err
			}
			if _, err := c.ListSecrets(ctx, ListRequest{ProjectID: "proj-1", Environment: "prod", Path: "/"}); err != nil {
				errs <- err
			}
			if _, err := c.ListFolders(ctx, FolderListRequest{ProjectID: "proj-1", Environment: "prod", Path: "/"}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent call failed: %v", err)
	}
}
