package reconcile

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alekc/infisical-mirror/internal/config"
	"github.com/alekc/infisical-mirror/internal/infisical"
	"github.com/alekc/infisical-mirror/internal/state"
)

type fakeReader struct {
	id      string
	secrets []infisical.Secret
	err     error

	resolved []string
	requests []infisical.ListRequest
}

func (f *fakeReader) ResolveProjectID(_ context.Context, slug string, environments ...string) (string, error) {
	f.resolved = append(f.resolved, slug+":"+strings.Join(environments, ","))
	if f.err != nil {
		return "", f.err
	}
	return f.id, nil
}

func (f *fakeReader) ListSecrets(_ context.Context, req infisical.ListRequest) ([]infisical.Secret, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.secrets, nil
}

// fakeStore fails any attempt to write, which is what a `plan` command must
// never do.
type fakeStore struct {
	t  *testing.T
	st *state.RuleState
	h  *state.Hasher
}

func (f *fakeStore) Load(context.Context, string) (*state.RuleState, error) {
	if f.st == nil {
		st := state.NewRuleState()
		st.FirstRun = true
		return st, nil
	}
	return f.st, nil
}

func (f *fakeStore) Save(context.Context, string, *state.RuleState) error {
	f.t.Fatal("planning saved state; a plan must not write anything")
	return nil
}

func (f *fakeStore) Hasher(context.Context) (*state.Hasher, error) { return f.h, nil }
func (f *fakeStore) Close() error                                  { return nil }

func planPair() config.Pair {
	p := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeleteIgnore)
	p.A = config.Scope{Instance: "cloud", Project: "cloud-proj", Env: "prod", Path: "/arr-stack"}
	p.B = config.Scope{Instance: "self", Project: "self-proj", Env: "staging", Path: "/apps/arr-stack"}
	return p
}

func TestPlanComparesAcrossDifferentRoots(t *testing.T) {
	h := testHasher(t)
	// The same two secrets, under roots that do not match, one of them a
	// subfolder. Comparing on the path relative to each side's own root is
	// what makes the two trees the same tree.
	a := &fakeReader{id: "id-a", secrets: []infisical.Secret{
		{Key: "API_KEY", Value: "v1", Path: "/arr-stack"},
		{Key: "DB_URL", Value: "v2", Path: "/arr-stack/sub"},
	}}
	b := &fakeReader{id: "id-b", secrets: []infisical.Secret{
		{Key: "API_KEY", Value: "v1", Path: "/apps/arr-stack"},
		{Key: "DB_URL", Value: "v2", Path: "/apps/arr-stack/sub"},
	}}

	p := NewPlanner(map[string]Reader{"cloud": a, "self": b}, &fakeStore{t: t, h: h}, WithClock(func() time.Time { return now }))
	plan, err := p.Plan(context.Background(), planPair())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if len(plan.Actions) != 0 {
		t.Errorf("actions = %v, want none: the two trees already agree", plan.Actions)
	}
	if plan.Converged != 2 {
		t.Errorf("converged = %d, want 2", plan.Converged)
	}
	if plan.SecretsA != 2 || plan.SecretsB != 2 {
		t.Errorf("counts = %d/%d, want 2/2", plan.SecretsA, plan.SecretsB)
	}
	if !plan.FirstRun {
		t.Error("FirstRun = false, want true")
	}

	// Each side is read at its own root, in its own environment, with the
	// project slug resolved rather than used as an id.
	if got := a.requests[0]; got.Path != "/arr-stack" || got.Environment != "prod" || got.ProjectID != "id-a" || !got.Recursive {
		t.Errorf("side a listed with %+v", got)
	}
	if got := b.requests[0]; got.Path != "/apps/arr-stack" || got.Environment != "staging" || got.ProjectID != "id-b" {
		t.Errorf("side b listed with %+v", got)
	}
	// The environment is passed to the resolve call, which is where a
	// misspelling is caught: an environment that does not exist lists
	// exactly like an empty one.
	if got := a.resolved; len(got) != 1 || got[0] != "cloud-proj:prod" {
		t.Errorf("resolved %v, want the slug and the environment", got)
	}
}

func TestPlanAppliesExcludes(t *testing.T) {
	h := testHasher(t)
	a := &fakeReader{id: "id-a", secrets: []infisical.Secret{
		{Key: "API_KEY", Value: "v1", Path: "/arr-stack"},
		{Key: "SCRATCH", Value: "v2", Path: "/arr-stack/scratch"},
	}}
	b := &fakeReader{id: "id-b"}

	pair := planPair()
	pair.Exclude = []string{"/scratch/**"}

	p := NewPlanner(map[string]Reader{"cloud": a, "self": b}, &fakeStore{t: t, h: h}, WithClock(func() time.Time { return now }))
	plan, err := p.Plan(context.Background(), pair)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if plan.SecretsA != 1 {
		t.Fatalf("SecretsA = %d, want the excluded folder filtered out", plan.SecretsA)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Key != "API_KEY" {
		t.Errorf("actions = %v, want only the selected key", plan.Actions)
	}
}

func TestPlanStopsAtNonRecursiveRoot(t *testing.T) {
	h := testHasher(t)
	a := &fakeReader{id: "id-a", secrets: []infisical.Secret{
		{Key: "API_KEY", Value: "v1", Path: "/arr-stack"},
		{Key: "NESTED", Value: "v2", Path: "/arr-stack/sub"},
	}}
	b := &fakeReader{id: "id-b"}

	pair := planPair()
	pair.Recursive = false

	p := NewPlanner(map[string]Reader{"cloud": a, "self": b}, &fakeStore{t: t, h: h}, WithClock(func() time.Time { return now }))
	plan, err := p.Plan(context.Background(), pair)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if a.requests[0].Recursive {
		t.Error("listed recursively for a non-recursive rule")
	}
	// The server is asked not to recurse, but a server that recursed anyway
	// must not get subfolder secrets into the plan through the back door.
	if plan.SecretsA != 1 || len(plan.Actions) != 1 || plan.Actions[0].Key != "API_KEY" {
		t.Errorf("actions = %v, want only the root folder's key", plan.Actions)
	}
}

func TestPlanNamesTheFailingSide(t *testing.T) {
	h := testHasher(t)
	a := &fakeReader{id: "id-a"}
	b := &fakeReader{err: errRead}

	p := NewPlanner(map[string]Reader{"cloud": a, "self": b}, &fakeStore{t: t, h: h}, WithClock(func() time.Time { return now }))
	_, err := p.Plan(context.Background(), planPair())
	if err == nil {
		t.Fatal("Plan succeeded with an unreadable side")
	}
	for _, want := range []string{"rule test", "side b", "self-proj", "staging"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestPlanRejectsAnUnknownInstance(t *testing.T) {
	h := testHasher(t)
	p := NewPlanner(map[string]Reader{"self": &fakeReader{}}, &fakeStore{t: t, h: h})
	_, err := p.Plan(context.Background(), planPair())
	if err == nil || !strings.Contains(err.Error(), "cloud") {
		t.Fatalf("err = %v, want it to name the missing instance", err)
	}
}

var errRead = &readError{}

type readError struct{}

func (*readError) Error() string { return "read failed" }
