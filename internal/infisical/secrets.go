package infisical

import (
	"context"
	"fmt"
	"iter"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// batchSize caps how many secrets go into one batch request. The API takes an
// unbounded array; a bounded one keeps a single failure from covering the whole
// estate and keeps request bodies reviewable in a log of what was attempted.
const batchSize = 100

// listLimit is the bound put on a secret listing, so that a truncated reply is
// detectable rather than merely smaller. It is far above any folder this tool
// is pointed at; it exists to be a tripwire, not a page size.
const listLimit = 10000

// Secret is one shared secret as the mirror cares about it: key, value,
// comment, and where it lives.
type Secret struct {
	ID    string
	Key   string
	Value string `json:"-"`
	// Comment is mirrored alongside the value. Tags, reminders and metadata are
	// deliberately not: they are per-project IDs and need a mapping layer.
	Comment string `json:"-"`
	// Path is absolute on the instance, the folder holding this secret.
	Path      string
	Version   int
	UpdatedAt time.Time
}

// String keeps the value out of anything that prints this secret, including a
// slice of them. GoString covers %#v, which ignores String and would otherwise
// print the value by reflection.
func (s Secret) String() string {
	return fmt.Sprintf("infisical.Secret{Key: %q, Path: %q, Version: %d, Value: <redacted>}", s.Key, s.Path, s.Version)
}

// GoString keeps the value out of %#v.
func (s Secret) GoString() string { return s.String() }

// SecretWrite is one secret to create or update.
type SecretWrite struct {
	Key     string
	Value   string `json:"-"`
	Comment string `json:"-"`
}

// String keeps the value out of anything that prints a pending write. A
// WriteRequest nests a batch of these, so one %+v on a request would otherwise
// print up to a hundred secret values at once.
func (s SecretWrite) String() string {
	return fmt.Sprintf("infisical.SecretWrite{Key: %q, Value: <redacted>}", s.Key)
}

// GoString keeps the value out of %#v.
func (s SecretWrite) GoString() string { return s.String() }

// ListRequest asks for the secrets under Path.
type ListRequest struct {
	ProjectID   string
	Environment string
	Path        string
	Recursive   bool
}

// WriteRequest is a batch of secrets destined for one folder.
type WriteRequest struct {
	ProjectID   string
	Environment string
	Path        string
	Secrets     []SecretWrite
}

// DeleteRequest names secrets to remove from one folder.
type DeleteRequest struct {
	ProjectID   string
	Environment string
	Path        string
	Keys        []string
}

type apiSecret struct {
	ID      string `json:"id"`
	Key     string `json:"secretKey"`
	Value   string `json:"secretValue"`
	Comment string `json:"secretComment"`
	Path    string `json:"secretPath"`
	Type    string `json:"type"`
	Version int    `json:"version"`
	// A pointer, so "the server said false" and "the server did not say" are
	// distinguishable. An unreadable secret arrives with an empty value, so if
	// this field is renamed, proxied away or absent on an older instance, a
	// plain bool decodes as false and the blank is mirrored over a real secret.
	ValueHidden   *bool  `json:"secretValueHidden"`
	UpdatedAtText string `json:"updatedAt"`
}

// ListSecrets returns the shared secrets under req.Path, recursive on request,
// each carrying the folder it actually lives in. expandSecretReferences,
// includeImports and includePersonalOverrides all default to true server-side
// and are switched off here on purpose. See docs/design.md.
func (c *Client) ListSecrets(ctx context.Context, req ListRequest) ([]Secret, error) {
	base := normalizePath(req.Path)
	query := url.Values{
		"projectId":                {req.ProjectID},
		"environment":              {req.Environment},
		"secretPath":               {base},
		"recursive":                {strconv.FormatBool(req.Recursive)},
		"viewSecretValue":          {"true"},
		"expandSecretReferences":   {"false"},
		"includeImports":           {"false"},
		"includePersonalOverrides": {"false"},
		"limit":                    {strconv.Itoa(listLimit)},
	}

	var resp struct {
		Secrets []apiSecret `json:"secrets"`
	}
	if err := c.do(ctx, "GET", "/api/v4/secrets", query, nil, &resp); err != nil {
		return nil, err
	}
	// A truncated listing is indistinguishable from a folder whose secrets were
	// all deleted, and nothing in the response says how many there were, so the
	// only check is to ask for a bound and refuse a reply that reaches it. The
	// request half is weak: unknown parameters are ignored. See docs/design.md.
	if len(resp.Secrets) >= listLimit {
		return nil, fmt.Errorf("infisical: %s in %s returned %d secrets, at or over the %d the request asked for, "+
			"so the listing may be truncated and is not safe to compare against",
			base, req.Environment, len(resp.Secrets), listLimit)
	}

	var hidden, ambiguous []string
	secrets := make([]Secret, 0, len(resp.Secrets))
	for _, s := range resp.Secrets {
		// Belt and braces: the query asks for shared values only, so a personal
		// row here would be a server-side change, not something to mirror.
		if s.Type != "" && s.Type != "shared" {
			continue
		}
		// A hidden value reads as an empty string, which is indistinguishable
		// from a secret whose value really is empty. Mirroring that would
		// blank the far side, so it is an error rather than a datum.
		switch {
		case s.ValueHidden != nil && *s.ValueHidden:
			hidden = append(hidden, secretRef(s.Path, base, s.Key))
			continue
		case s.ValueHidden == nil && s.Value == "":
			// The server did not say whether this value is readable, and it is
			// empty. An empty secret and one this identity may not read look
			// identical from here, so there is nothing to do but stop.
			ambiguous = append(ambiguous, secretRef(s.Path, base, s.Key))
			continue
		}

		path := base
		if s.Path != "" {
			path = normalizePath(s.Path)
		}
		secrets = append(secrets, Secret{
			ID:        s.ID,
			Key:       s.Key,
			Value:     NormalizeValue(s.Value),
			Comment:   NormalizeComment(s.Comment),
			Path:      path,
			Version:   s.Version,
			UpdatedAt: parseTime(s.UpdatedAtText),
		})
	}

	if len(hidden) > 0 {
		sort.Strings(hidden)
		return nil, fmt.Errorf("infisical: %d secret(s) returned with the value hidden, so this identity cannot read them: %s",
			len(hidden), strings.Join(hidden, ", "))
	}
	if len(ambiguous) > 0 {
		sort.Strings(ambiguous)
		return nil, fmt.Errorf("infisical: %d secret(s) came back empty with no secretValueHidden field, "+
			"so an empty secret cannot be told from one this identity may not read: %s",
			len(ambiguous), strings.Join(ambiguous, ", "))
	}
	return secrets, nil
}

// CreateSecrets creates secrets in one folder, which must already exist.
func (c *Client) CreateSecrets(ctx context.Context, req WriteRequest) error {
	return c.writeBatch(ctx, "POST", req)
}

// UpdateSecrets overwrites the value and comment of existing secrets in one
// folder.
func (c *Client) UpdateSecrets(ctx context.Context, req WriteRequest) error {
	return c.writeBatch(ctx, "PATCH", req)
}

func (c *Client) writeBatch(ctx context.Context, method string, req WriteRequest) error {
	if len(req.Secrets) == 0 {
		return nil
	}
	path := normalizePath(req.Path)

	for chunk := range chunks(req.Secrets, batchSize) {
		payload := make([]map[string]any, 0, len(chunk))
		for _, s := range chunk {
			if s.Key == "" {
				return fmt.Errorf("infisical: a secret with an empty key was queued for %s in %s", path, req.Environment)
			}
			// secretComment is sent on every write, including when empty.
			// Omitting it leaves the destination's old comment in place, so a
			// comment could be set and changed but never cleared, and the two
			// sides would never converge. See docs/design.md.
			payload = append(payload, map[string]any{
				"secretKey":     s.Key,
				"secretValue":   NormalizeValue(s.Value),
				"secretComment": NormalizeComment(s.Comment),
			})
		}

		body := map[string]any{
			"projectId":   req.ProjectID,
			"environment": req.Environment,
			"secretPath":  path,
			"secrets":     payload,
		}
		if err := c.do(ctx, method, "/api/v4/secrets/batch", nil, body, nil); err != nil {
			return err
		}
	}
	return nil
}

// DeleteSecrets removes secrets from one folder.
func (c *Client) DeleteSecrets(ctx context.Context, req DeleteRequest) error {
	if len(req.Keys) == 0 {
		return nil
	}
	path := normalizePath(req.Path)

	for chunk := range chunks(req.Keys, batchSize) {
		payload := make([]map[string]string, 0, len(chunk))
		for _, key := range chunk {
			if key == "" {
				return fmt.Errorf("infisical: an empty key was queued for deletion in %s in %s", path, req.Environment)
			}
			payload = append(payload, map[string]string{"secretKey": key})
		}

		body := map[string]any{
			"projectId":   req.ProjectID,
			"environment": req.Environment,
			"secretPath":  path,
			"secrets":     payload,
		}
		if err := c.do(ctx, "DELETE", "/api/v4/secrets/batch", nil, body, nil); err != nil {
			return err
		}
	}
	return nil
}

// NormalizeValue applies the same transform the server applies on write:
// JavaScript's String.trim() on both ends, keeping a single trailing newline if
// the value had one. It applies on read too, or a padded value hashes one way
// locally and another once stored, and is rewritten forever. See design.md.
func NormalizeValue(v string) string {
	trimmed := strings.TrimSpace(v)
	if strings.HasSuffix(v, "\n") {
		return trimmed + "\n"
	}
	return trimmed
}

// NormalizeComment does for a comment what NormalizeValue does for a value, and
// for the same reason: a comment is inside the hash. The API trims it on create
// and not on update, so a padded one would be rewritten every pass. Unlike a
// value it keeps no trailing newline. See docs/design.md.
func NormalizeComment(c string) string { return strings.TrimSpace(c) }

// chunks yields successive slices of at most size elements. A size below one
// would loop without advancing, so it is treated as one.
func chunks[T any](items []T, size int) iter.Seq[[]T] {
	size = max(size, 1)
	return func(yield func([]T) bool) {
		for start := 0; start < len(items); start += size {
			end := min(start+size, len(items))
			if !yield(items[start:end]) {
				return
			}
		}
	}
}

// secretRef names a secret for an error message: path plus key, never a value.
func secretRef(path, fallback, key string) string {
	if path == "" {
		path = fallback
	}
	return normalizePath(path) + ":" + key
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
