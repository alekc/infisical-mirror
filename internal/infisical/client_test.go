package infisical

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedRequest is one request the fake server saw, kept in a form tests can
// assert on without re-reading the body.
type recordedRequest struct {
	Method string
	Path   string
	Query  map[string]string
	Auth   string
	Body   map[string]any
}

// fakeServer is an httptest server plus the log of what reached it.
type fakeServer struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
}

// newFakeServer starts a server whose handler is the supplied function, and
// records every request first.
func newFakeServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, seen int)) *fakeServer {
	t.Helper()

	fake := &fakeServer{}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		rec := recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  map[string]string{},
			Auth:   r.Header.Get("Authorization"),
		}
		for k := range r.URL.Query() {
			rec.Query[k] = r.URL.Query().Get(k)
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &rec.Body)
		}

		fake.mu.Lock()
		seen := len(fake.requests)
		fake.requests = append(fake.requests, rec)
		fake.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		handler(w, r, seen)
	}))
	t.Cleanup(fake.Close)
	return fake
}

func (f *fakeServer) seen() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// only returns the single recorded request, failing when there was not exactly
// one: an assertion about "the request" is meaningless if there were three.
func (f *fakeServer) only(t *testing.T) recordedRequest {
	t.Helper()
	got := f.seen()
	if len(got) != 1 {
		t.Fatalf("want exactly 1 request, got %d", len(got))
	}
	return got[0]
}

// newTestClient builds a client against the fake with a static token and a
// no-op sleep, so retry tests do not actually wait.
func newTestClient(t *testing.T, fake *fakeServer) *Client {
	t.Helper()

	tok, err := NewStaticToken("test-token")
	if err != nil {
		t.Fatalf("NewStaticToken: %v", err)
	}
	c, err := New(fake.URL, tok, WithClock(time.Now, func(time.Duration) {}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encoding fake response: %v", err)
	}
}

func TestNewRejectsBadInput(t *testing.T) {
	tok, err := NewStaticToken("t")
	if err != nil {
		t.Fatalf("NewStaticToken: %v", err)
	}

	for name, url := range map[string]string{
		"empty":       "",
		"no scheme":   "app.infisical.com",
		"wrong shape": "ftp://app.infisical.com",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(url, tok); err == nil {
				t.Fatalf("want an error for url %q", url)
			}
		})
	}

	if _, err := New("https://app.infisical.com", nil); err == nil {
		t.Fatal("want an error when no token source is supplied")
	}

	c, err := New("https://app.infisical.com/", tok)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.baseURL != "https://app.infisical.com" {
		t.Fatalf("trailing slash not trimmed: %q", c.baseURL)
	}
}

// The three defaulted-true query parameters are the load-bearing part of this
// call: leaving any of them at its default silently changes what gets mirrored.
func TestListSecretsSendsSafeDefaults(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(t, w, 200, map[string]any{"secrets": []any{}})
	})

	_, err := newTestClient(t, fake).ListSecrets(context.Background(), ListRequest{
		ProjectID:   "proj-1",
		Environment: "prod",
		Path:        "/apps/",
		Recursive:   true,
	})
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}

	req := fake.only(t)
	if req.Method != "GET" || req.Path != "/api/v4/secrets" {
		t.Fatalf("want GET /api/v4/secrets, got %s %s", req.Method, req.Path)
	}
	if req.Auth != "Bearer test-token" {
		t.Fatalf("Authorization header = %q", req.Auth)
	}

	want := map[string]string{
		"projectId":                "proj-1",
		"environment":              "prod",
		"secretPath":               "/apps",
		"recursive":                "true",
		"viewSecretValue":          "true",
		"expandSecretReferences":   "false",
		"includeImports":           "false",
		"includePersonalOverrides": "false",
		"limit":                    strconv.Itoa(listLimit),
	}
	// The whole set, not a subset. An unrecognised query parameter is ignored
	// by the API rather than rejected, so a typo in one of these names reverts
	// it to its server-side default and nothing anywhere says so: this
	// assertion is the only place a misspelt viewSecretValue is visible.
	if !maps.Equal(req.Query, want) {
		t.Errorf("query = %v,\nwant %v", req.Query, want)
	}
}

func TestListSecretsParsesRows(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(t, w, 200, map[string]any{"secrets": []map[string]any{
			{
				"id": "s1", "secretKey": "ROOT", "secretValue": "a", "type": "shared",
				"version": 3, "updatedAt": "2026-09-18T10:00:00Z",
			},
			{
				"id": "s2", "secretKey": "NESTED", "secretValue": "  b\n", "secretComment": "note",
				"secretPath": "/apps/arr", "type": "shared", "version": 1,
			},
			{"id": "s3", "secretKey": "PERSONAL", "secretValue": "mine", "type": "personal"},
		}})
	})

	got, err := newTestClient(t, fake).ListSecrets(context.Background(), ListRequest{
		ProjectID: "proj-1", Environment: "prod", Path: "/apps", Recursive: true,
	})
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 shared secrets, got %d: %+v", len(got), got)
	}

	// A row with no secretPath belongs to the folder that was queried.
	if got[0].Path != "/apps" || got[0].Key != "ROOT" || got[0].Version != 3 {
		t.Errorf("first secret = %+v", got[0])
	}
	if got[0].UpdatedAt.IsZero() {
		t.Error("updatedAt was not parsed")
	}
	// Values arrive normalised, so a hash taken here matches one taken after
	// the server has stored the same value.
	if got[1].Path != "/apps/arr" || got[1].Value != "b\n" || got[1].Comment != "note" {
		t.Errorf("second secret = %+v", got[1])
	}
}

// A hidden value is an empty string on the wire, which would mirror as "blank
// the far side". It has to fail instead.
func TestListSecretsRejectsHiddenValues(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(t, w, 200, map[string]any{"secrets": []map[string]any{
			{"secretKey": "VISIBLE", "secretValue": "v", "type": "shared"},
			{"secretKey": "HIDDEN", "secretValue": "", "secretValueHidden": true, "secretPath": "/apps/arr", "type": "shared"},
		}})
	})

	_, err := newTestClient(t, fake).ListSecrets(context.Background(), ListRequest{
		ProjectID: "proj-1", Environment: "prod", Path: "/apps",
	})
	if err == nil {
		t.Fatal("want an error when a value is hidden")
	}
	if !strings.Contains(err.Error(), "/apps/arr:HIDDEN") {
		t.Errorf("error should name the unreadable secret, got: %v", err)
	}
	if strings.Contains(err.Error(), "\"v\"") {
		t.Errorf("error leaked a secret value: %v", err)
	}
}

func TestWriteBatchChunksAndShapesBody(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(t, w, 200, map[string]any{"secrets": []any{}})
	})

	secrets := make([]SecretWrite, 250)
	for i := range secrets {
		secrets[i] = SecretWrite{Key: "K" + string(rune('A'+i%26)) + string(rune('0'+i/26)), Value: " v "}
	}
	secrets[0].Comment = "keep me"

	err := newTestClient(t, fake).CreateSecrets(context.Background(), WriteRequest{
		ProjectID: "proj-1", Environment: "prod", Path: "/apps/arr/", Secrets: secrets,
	})
	if err != nil {
		t.Fatalf("CreateSecrets: %v", err)
	}

	got := fake.seen()
	if len(got) != 3 {
		t.Fatalf("250 secrets at a batch size of %d should be 3 requests, got %d", batchSize, len(got))
	}

	first := got[0]
	if first.Method != "POST" || first.Path != "/api/v4/secrets/batch" {
		t.Fatalf("want POST /api/v4/secrets/batch, got %s %s", first.Method, first.Path)
	}
	if first.Body["secretPath"] != "/apps/arr" {
		t.Errorf("secretPath = %v, want the trailing slash trimmed", first.Body["secretPath"])
	}

	body, _ := first.Body["secrets"].([]any)
	if len(body) != batchSize {
		t.Fatalf("first chunk has %d secrets, want %d", len(body), batchSize)
	}
	entry, _ := body[0].(map[string]any)
	if entry["secretValue"] != "v" {
		t.Errorf("value was not normalised before the write: %v", entry["secretValue"])
	}
	if entry["secretComment"] != "keep me" {
		t.Errorf("comment = %v", entry["secretComment"])
	}

	last, _ := got[2].Body["secrets"].([]any)
	if len(last) != 50 {
		t.Errorf("last chunk has %d secrets, want 50", len(last))
	}
}

func TestUpdateAndDeleteUseTheRightVerbs(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(t, w, 200, map[string]any{})
	})
	c := newTestClient(t, fake)
	ctx := context.Background()

	if err := c.UpdateSecrets(ctx, WriteRequest{
		ProjectID: "p", Environment: "prod", Path: "/", Secrets: []SecretWrite{{Key: "A", Value: "1"}},
	}); err != nil {
		t.Fatalf("UpdateSecrets: %v", err)
	}
	if err := c.DeleteSecrets(ctx, DeleteRequest{
		ProjectID: "p", Environment: "prod", Path: "/", Keys: []string{"A", "B"},
	}); err != nil {
		t.Fatalf("DeleteSecrets: %v", err)
	}

	got := fake.seen()
	if len(got) != 2 {
		t.Fatalf("want 2 requests, got %d", len(got))
	}
	if got[0].Method != "PATCH" {
		t.Errorf("update used %s, want PATCH", got[0].Method)
	}
	if got[1].Method != "DELETE" {
		t.Errorf("delete used %s, want DELETE", got[1].Method)
	}

	keys, _ := got[1].Body["secrets"].([]any)
	if len(keys) != 2 {
		t.Fatalf("delete body carries %d entries, want 2", len(keys))
	}
	entry, _ := keys[0].(map[string]any)
	if entry["secretKey"] != "A" {
		t.Errorf("delete entry = %v, want a bare secretKey", entry)
	}
	if _, ok := entry["secretValue"]; ok {
		t.Error("delete entry should not carry a value")
	}
}

// An empty batch is a no-op, not a request: the API rejects an empty array, so
// sending one would turn "nothing to do" into a failed run.
func TestEmptyBatchesSendNothing(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		t.Error("no request should have been sent")
		writeJSON(t, w, 200, map[string]any{})
	})
	c := newTestClient(t, fake)
	ctx := context.Background()

	if err := c.CreateSecrets(ctx, WriteRequest{ProjectID: "p", Environment: "prod", Path: "/"}); err != nil {
		t.Fatalf("CreateSecrets: %v", err)
	}
	if err := c.DeleteSecrets(ctx, DeleteRequest{ProjectID: "p", Environment: "prod", Path: "/"}); err != nil {
		t.Fatalf("DeleteSecrets: %v", err)
	}
	if n := len(fake.seen()); n != 0 {
		t.Fatalf("%d requests were sent for empty batches", n)
	}
}

func TestErrorClassification(t *testing.T) {
	tests := map[string]struct {
		status    int
		body      string
		wantCalls int
		check     func(*testing.T, error)
	}{
		"404 is not found and is not retried": {
			status: 404, body: `{"message":"Folder with path '/apps' in environment with slug 'prod' not found"}`,
			wantCalls: 1,
			check: func(t *testing.T, err error) {
				if !IsNotFound(err) {
					t.Errorf("IsNotFound = false for: %v", err)
				}
				if !strings.Contains(err.Error(), "Folder with path") {
					t.Errorf("server message was dropped: %v", err)
				}
			},
		},
		"400 is not retried": {
			status: 400, body: `{"error":"bad request"}`, wantCalls: 1,
			check: func(t *testing.T, err error) {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Retryable() {
					t.Errorf("400 should be a non-retryable APIError, got: %v", err)
				}
			},
		},
		"429 is retried to exhaustion": {
			status: 429, body: `{"message":"slow down"}`, wantCalls: maxAttempts,
			check: func(t *testing.T, err error) {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || !apiErr.Retryable() {
					t.Errorf("429 should be retryable, got: %v", err)
				}
			},
		},
		"500 is retried to exhaustion": {
			status: 500, body: `boom`, wantCalls: maxAttempts,
			check: func(t *testing.T, err error) {
				// A body that does not parse is described, never echoed: on a
				// secrets endpoint an echoed body hands the request, values and
				// all, back into the log the operator is about to paste.
				if strings.Contains(err.Error(), "boom") {
					t.Errorf("an unparseable body was echoed into the error: %v", err)
				}
				if !strings.Contains(err.Error(), "unparseable 4-byte body") {
					t.Errorf("the error should describe the body it could not parse: %v", err)
				}
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})

			_, err := newTestClient(t, fake).ListSecrets(context.Background(), ListRequest{
				ProjectID: "p", Environment: "prod", Path: "/",
			})
			if err == nil {
				t.Fatal("want an error")
			}
			if n := len(fake.seen()); n != tc.wantCalls {
				t.Errorf("server saw %d calls, want %d", n, tc.wantCalls)
			}
			tc.check(t, err)
		})
	}
}

func TestRetryThenSuccess(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, seen int) {
		if seen < 2 {
			w.WriteHeader(503)
			return
		}
		writeJSON(t, w, 200, map[string]any{"secrets": []map[string]any{
			{"secretKey": "A", "secretValue": "1", "type": "shared"},
		}})
	})

	got, err := newTestClient(t, fake).ListSecrets(context.Background(), ListRequest{
		ProjectID: "p", Environment: "prod", Path: "/",
	})
	if err != nil {
		t.Fatalf("ListSecrets after two 503s: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 secret, got %d", len(got))
	}
	if n := len(fake.seen()); n != 3 {
		t.Errorf("server saw %d calls, want 3", n)
	}
}

// A retried write must send the same body every time: a reader consumed on the
// first attempt would make the second one an empty batch.
func TestRetriedWriteReplaysTheBody(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, seen int) {
		if seen == 0 {
			w.WriteHeader(500)
			return
		}
		writeJSON(t, w, 200, map[string]any{})
	})

	err := newTestClient(t, fake).CreateSecrets(context.Background(), WriteRequest{
		ProjectID: "p", Environment: "prod", Path: "/", Secrets: []SecretWrite{{Key: "A", Value: "1"}},
	})
	if err != nil {
		t.Fatalf("CreateSecrets: %v", err)
	}

	got := fake.seen()
	if len(got) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(got))
	}
	for i, req := range got {
		secrets, _ := req.Body["secrets"].([]any)
		if len(secrets) != 1 {
			t.Errorf("attempt %d sent %d secrets, want 1", i+1, len(secrets))
		}
	}
}

func TestNormalizeValue(t *testing.T) {
	tests := map[string]struct{ in, want string }{
		"untouched":            {"value", "value"},
		"surrounding spaces":   {"  value  ", "value"},
		"trailing newline":     {"value\n", "value\n"},
		"padded with newline":  {"  value  \n", "value\n"},
		"many trailing blanks": {"value\n\n\n", "value\n"},
		"newline only":         {"\n", "\n"},
		"blank":                {"   ", ""},
		"empty":                {"", ""},
		"inner newlines kept":  {"line1\nline2", "line1\nline2"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := NormalizeValue(tc.in); got != tc.want {
				t.Errorf("NormalizeValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// The server applies this on write, so applying it twice must be
			// the same as applying it once or the mirror never converges.
			if got := NormalizeValue(NormalizeValue(tc.in)); got != tc.want {
				t.Errorf("NormalizeValue is not idempotent for %q: %q", tc.in, got)
			}
		})
	}
}

// A truncated listing is indistinguishable from a folder whose secrets were all
// deleted, and under a propagate-deletes policy the mirror acts on that
// difference. Nothing in the response says how many there were in total, so the
// only check is to ask for a bound and refuse a reply that reaches it.
func TestListSecretsRefusesAPossiblyTruncatedReply(t *testing.T) {
	rows := make([]map[string]any, listLimit)
	for i := range rows {
		rows[i] = map[string]any{
			"secretKey": "K" + strconv.Itoa(i), "secretValue": "v", "type": "shared",
		}
	}
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(t, w, 200, map[string]any{"secrets": rows})
	})

	_, err := newTestClient(t, fake).ListSecrets(context.Background(), ListRequest{
		ProjectID: "p", Environment: "prod", Path: "/apps",
	})
	if err == nil {
		t.Fatal("want an error when the reply reaches the limit it asked for")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error should say why it refused, got: %v", err)
	}

	// One under the limit is a complete listing and must pass, or the tripwire
	// is a hard cap on how big a folder the tool can mirror.
	fake2 := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(t, w, 200, map[string]any{"secrets": rows[:listLimit-1]})
	})
	got, err := newTestClient(t, fake2).ListSecrets(context.Background(), ListRequest{
		ProjectID: "p", Environment: "prod", Path: "/apps",
	})
	if err != nil {
		t.Fatalf("a full-but-not-truncated listing was refused: %v", err)
	}
	if len(got) != listLimit-1 {
		t.Errorf("got %d secrets, want %d", len(got), listLimit-1)
	}
}

// An empty value with no secretValueHidden field is ambiguous: a secret whose
// value really is empty and one this identity may not read look identical from
// here. The field is a pointer precisely so the two are distinguishable, and
// "the server did not say" has to stop the run rather than mirror the blank.
func TestListSecretsRefusesAnUnexplainedEmptyValue(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(t, w, 200, map[string]any{"secrets": []map[string]any{
			{"secretKey": "VISIBLE", "secretValue": "v", "type": "shared"},
			{"secretKey": "BLANK", "secretValue": "", "secretPath": "/apps/arr", "type": "shared"},
		}})
	})

	_, err := newTestClient(t, fake).ListSecrets(context.Background(), ListRequest{
		ProjectID: "p", Environment: "prod", Path: "/apps",
	})
	if err == nil {
		t.Fatal("want an error when an empty value is unexplained")
	}
	if !strings.Contains(err.Error(), "/apps/arr:BLANK") {
		t.Errorf("error should name the ambiguous secret, got: %v", err)
	}

	// An explicit false is the server answering the question, so an empty value
	// alongside it is a real empty secret and passes.
	fake2 := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(t, w, 200, map[string]any{"secrets": []map[string]any{
			{"secretKey": "BLANK", "secretValue": "", "secretValueHidden": false, "type": "shared"},
		}})
	})
	got, err := newTestClient(t, fake2).ListSecrets(context.Background(), ListRequest{
		ProjectID: "p", Environment: "prod", Path: "/apps",
	})
	if err != nil {
		t.Fatalf("an explicitly readable empty value was refused: %v", err)
	}
	if len(got) != 1 || got[0].Value != "" {
		t.Errorf("got %+v, want one empty secret", got)
	}
}

// Comments round-trip through normalisation identically on both sides, and a
// write always names the comment even when it is empty. secretComment is
// trimmed on create and untrimmed on update, and an absent one on update leaves
// the old value in place. See docs/design.md.
func TestWritesAlwaysCarryANormalisedComment(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(t, w, 200, map[string]any{})
	})
	c := newTestClient(t, fake)

	err := c.UpdateSecrets(context.Background(), WriteRequest{
		ProjectID: "p", Environment: "prod", Path: "/",
		Secrets: []SecretWrite{
			{Key: "PADDED", Value: "v", Comment: "  rotated 2026-09  "},
			{Key: "CLEARED", Value: "v"},
		},
	})
	if err != nil {
		t.Fatalf("UpdateSecrets: %v", err)
	}

	body, _ := fake.only(t).Body["secrets"].([]any)
	if len(body) != 2 {
		t.Fatalf("want 2 secrets in the body, got %d", len(body))
	}

	padded, _ := body[0].(map[string]any)
	if padded["secretComment"] != "rotated 2026-09" {
		t.Errorf("comment = %q, want it trimmed the same way the API trims on create", padded["secretComment"])
	}

	cleared, _ := body[1].(map[string]any)
	got, present := cleared["secretComment"]
	if !present {
		t.Error("secretComment was omitted, so an existing comment could never be cleared")
	}
	if got != "" {
		t.Errorf("cleared comment = %q, want an empty string", got)
	}
}

func TestNormalizeComment(t *testing.T) {
	tests := map[string]struct{ in, want string }{
		"untouched":          {"rotated", "rotated"},
		"surrounding spaces": {"  rotated  ", "rotated"},
		"trailing newline":   {"rotated\n", "rotated"},
		"blank":              {"  \n ", ""},
		"empty":              {"", ""},
		"inner spacing kept": {"line1\nline2", "line1\nline2"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := NormalizeComment(tc.in); got != tc.want {
				t.Errorf("NormalizeComment(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if got := NormalizeComment(NormalizeComment(tc.in)); got != tc.want {
				t.Errorf("NormalizeComment is not idempotent for %q: %q", tc.in, got)
			}
		})
	}
}
