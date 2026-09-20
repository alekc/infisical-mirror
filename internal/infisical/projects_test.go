package infisical

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// projectBody is the project as both live instances return it: at the top
// level, with no envelope. Fixtures wrapped it in a "project" key until
// 2026-09-18, when a real instance failed with "resolved to no id" and the
// fixture turned out to be the only thing that had agreed with the client.
func projectBody() map[string]any {
	return map[string]any{
		"id":   "11111111-2222-3333-4444-555555555555",
		"slug": "home-cluster-k-wv9",
		"name": "home cluster",
		"environments": []map[string]any{
			{"id": "e1", "slug": "prod", "name": "Production"},
			{"id": "e2", "slug": "dev", "name": "Development"},
		},
	}
}

// wrappedProjectResponse is tolerated as well, because the envelope is not
// something the endpoint promises and an unwrap that finds nothing reads
// exactly like a project that does not exist.
func wrappedProjectResponse() map[string]any {
	return map[string]any{"project": projectBody()}
}

func TestProjectBySlugAcceptsBothEnvelopes(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"unwrapped, as both live instances return it": projectBody(),
		"wrapped in a project key":                    wrappedProjectResponse(),
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				writeJSON(t, w, 200, body)
			})
			got, err := newTestClient(t, fake).ProjectBySlug(context.Background(), "home-cluster-k-wv9")
			if err != nil {
				t.Fatalf("ProjectBySlug: %v", err)
			}
			if got.ID != "11111111-2222-3333-4444-555555555555" {
				t.Errorf("id = %q", got.ID)
			}
			if !got.HasEnvironment("prod") {
				t.Errorf("environments = %v", got.EnvironmentSlugs())
			}
		})
	}
}

func TestProjectBySlugResolvesAndCaches(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(t, w, 200, projectBody())
	})

	c := newTestClient(t, fake)
	ctx := context.Background()

	got, err := c.ProjectBySlug(ctx, "home-cluster-k-wv9")
	if err != nil {
		t.Fatalf("ProjectBySlug: %v", err)
	}
	if got.ID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("id = %q", got.ID)
	}
	if req := fake.only(t); req.Path != "/api/v1/projects/slug/home-cluster-k-wv9" {
		t.Errorf("request path = %q", req.Path)
	}

	// A project id never changes under a run, so the second call must not hit
	// the API again.
	if _, err := c.ProjectBySlug(ctx, "home-cluster-k-wv9"); err != nil {
		t.Fatalf("second ProjectBySlug: %v", err)
	}
	if n := len(fake.seen()); n != 1 {
		t.Errorf("server saw %d calls, want 1", n)
	}

	if !got.HasEnvironment("prod") || got.HasEnvironment("staging") {
		t.Errorf("environments = %v", got.EnvironmentSlugs())
	}
	if want := []string{"dev", "prod"}; strings.Join(got.EnvironmentSlugs(), ",") != strings.Join(want, ",") {
		t.Errorf("EnvironmentSlugs = %v, want sorted %v", got.EnvironmentSlugs(), want)
	}
}

// A typo in an environment name would otherwise read as an empty environment,
// which a sync interprets as "everything here was deleted".
func TestResolveProjectIDValidatesEnvironments(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeJSON(t, w, 200, projectBody())
	})
	c := newTestClient(t, fake)
	ctx := context.Background()

	id, err := c.ResolveProjectID(ctx, "home-cluster-k-wv9", "prod", "dev")
	if err != nil {
		t.Fatalf("ResolveProjectID: %v", err)
	}
	if id != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("id = %q", id)
	}

	_, err = c.ResolveProjectID(ctx, "home-cluster-k-wv9", "prod", "staging", "qa")
	if err == nil {
		t.Fatal("want an error for environments that do not exist")
	}
	// The message has to name both what is missing and what is available, or
	// the next step is a guess.
	for _, want := range []string{"staging", "qa", "dev, prod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// A project id pasted where a slug belongs is the likely mistake, so the 404
// says which one the config wants.
func TestProjectBySlugNotFound(t *testing.T) {
	fake := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"message":"Project not found"}`)
	})

	_, err := newTestClient(t, fake).ProjectBySlug(context.Background(), "nope")
	if err == nil || !IsNotFound(err) {
		t.Fatalf("want a not-found error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "slug") {
		t.Errorf("error should explain that the config names a slug, got: %v", err)
	}

	if _, err := newTestClient(t, fake).ProjectBySlug(context.Background(), "  "); err == nil {
		t.Fatal("want an error for an empty slug")
	}
}
