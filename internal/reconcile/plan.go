package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/alekc/infisical-mirror/internal/config"
	"github.com/alekc/infisical-mirror/internal/infisical"
	"github.com/alekc/infisical-mirror/internal/state"
)

// Reader is the part of the Infisical client that planning needs. Narrowing it
// to two read calls is the point: a planner holding this interface has no
// write method to call, so `plan` is read-only by construction rather than by
// discipline.
type Reader interface {
	ResolveProjectID(ctx context.Context, slug string, environments ...string) (string, error)
	ListSecrets(ctx context.Context, req infisical.ListRequest) ([]infisical.Secret, error)
}

// Planner reads both sides of a pair and reconciles them.
type Planner struct {
	instances map[string]Reader
	store     state.Store
	now       func() time.Time
}

// PlannerOption configures a Planner.
type PlannerOption func(*Planner)

// WithClock replaces the planner's clock, for tests.
func WithClock(now func() time.Time) PlannerOption {
	return func(p *Planner) { p.now = now }
}

// NewPlanner returns a planner reading through the given instances, keyed by
// the instance names used in the config.
func NewPlanner(instances map[string]Reader, store state.Store, opts ...PlannerOption) *Planner {
	p := &Planner{instances: instances, store: store, now: time.Now}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Plan is one pair's outcome plus what it was computed from.
type Plan struct {
	*Outcome
	Pair config.Pair
	// Scope is the state scope this pair's entries live under.
	Scope string
	// SecretsA and SecretsB count the in-scope secrets read from each side,
	// after the rule's include and exclude patterns have been applied.
	SecretsA int
	SecretsB int
	// Tracked is the number of live keys state held before this run.
	Tracked int
	// FirstRun says there was no state for this pair at all, so nothing in
	// the plan could be told apart from a new secret.
	FirstRun bool
}

// Plan reads both sides of a pair and returns what it would take to bring them
// into step. It writes nothing, to either instance or to the state store.
func (p *Planner) Plan(ctx context.Context, pair config.Pair) (*Plan, error) {
	hasher, err := p.store.Hasher(ctx)
	if err != nil {
		return nil, fmt.Errorf("rule %s: %w", pair.Rule, err)
	}
	st, err := p.store.Load(ctx, pair.StateScope())
	if err != nil {
		return nil, fmt.Errorf("rule %s: %w", pair.Rule, err)
	}

	a, err := p.snapshot(ctx, pair, pair.A)
	if err != nil {
		return nil, fmt.Errorf("rule %s, side a (%s): %w", pair.Rule, pair.A, err)
	}
	b, err := p.snapshot(ctx, pair, pair.B)
	if err != nil {
		return nil, fmt.Errorf("rule %s, side b (%s): %w", pair.Rule, pair.B, err)
	}

	out, err := Reconcile(pair, a, b, st, hasher, p.now())
	if err != nil {
		return nil, fmt.Errorf("rule %s: %w", pair.Rule, err)
	}

	return &Plan{
		Outcome:  out,
		Pair:     pair,
		Scope:    pair.StateScope(),
		SecretsA: len(a),
		SecretsB: len(b),
		Tracked:  st.Tracked(),
		FirstRun: st.FirstRun,
	}, nil
}

// snapshot lists one side and reduces it to the keys the rule selects, keyed
// by their path relative to that side's root.
func (p *Planner) snapshot(ctx context.Context, pair config.Pair, scope config.Scope) (Snapshot, error) {
	client, ok := p.instances[scope.Instance]
	if !ok {
		return nil, fmt.Errorf("no client for instance %q", scope.Instance)
	}

	// Resolving the slug is also what checks the environment: an environment
	// that does not exist answers a list call exactly like an empty one, and
	// an empty environment is what a sync reads as a mass deletion.
	projectID, err := client.ResolveProjectID(ctx, scope.Project, scope.Env)
	if err != nil {
		return nil, err
	}

	secrets, err := client.ListSecrets(ctx, infisical.ListRequest{
		ProjectID:   projectID,
		Environment: scope.Env,
		Path:        scope.Path,
		Recursive:   pair.Recursive,
	})
	if err != nil {
		return nil, err
	}

	snap := make(Snapshot, len(secrets))
	for _, s := range secrets {
		// The listing asked for one subtree, so a row from outside it is the
		// server answering a different question. The relative path would be
		// wrong, and a wrong relative path writes the secret into the wrong
		// folder, which on a folder-scoped permission model changes its audience.
		rel, ok := config.RelativeTo(s.Path, scope.Path)
		if !ok {
			return nil, fmt.Errorf("%s returned a secret from %s, which is outside the requested %s",
				scope, s.Path, scope.Path)
		}
		if !pair.Selects(rel) {
			continue
		}
		key := state.EntryKey(rel, s.Key)
		if _, clash := snap[key]; clash {
			// Two secrets with one key in one folder is not something the
			// API should return, and silently keeping the last one would
			// mirror an arbitrary choice.
			return nil, fmt.Errorf("key %s appears twice in %s", s.Key, rel)
		}
		snap[key] = Secret{Value: s.Value, Comment: s.Comment, UpdatedAt: s.UpdatedAt}
	}
	return snap, nil
}
