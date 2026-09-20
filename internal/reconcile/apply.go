package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/alekc/infisical-mirror/internal/config"
	"github.com/alekc/infisical-mirror/internal/infisical"
	"github.com/alekc/infisical-mirror/internal/state"
)

// Writer is the part of the Infisical client that carrying out a plan needs. It
// lives beside Reader and nothing widens one into the other: a planner holds a
// Reader, the applier a Writer, so the read-only path stays read-only by the
// type it was handed rather than by the code remembering not to call something.
type Writer interface {
	ResolveProjectID(ctx context.Context, slug string, environments ...string) (string, error)
	EnsureFolder(ctx context.Context, projectID, environment, absPath string) error
	CreateSecrets(ctx context.Context, req infisical.WriteRequest) error
	UpdateSecrets(ctx context.Context, req infisical.WriteRequest) error
	DeleteSecrets(ctx context.Context, req infisical.DeleteRequest) error
}

// ApplyResult is what one rule's apply actually did, as opposed to what its
// plan said it would.
type ApplyResult struct {
	Created int
	Updated int
	Deleted int
	// Failed counts actions whose batch did not land. Their state entries are
	// put back as they were, so the next pass sees the same drift and retries.
	Failed int
	// Errors is one entry per failed batch, naming the folder and operation.
	Errors []error
	// State is the rule state as it should be persisted: the plan's next state
	// with every failed action's entry reverted. Nil when nothing should be
	// written, which is a dry run.
	State *state.RuleState
	// DryRun says this result describes a rule that was not carried out.
	DryRun bool
}

// Wrote reports whether anything reached an instance.
func (r *ApplyResult) Wrote() int { return r.Created + r.Updated + r.Deleted }

// ErrBlocked is returned when a guard fired and the caller did not override it.
var ErrBlocked = errors.New("reconcile: a guard refused this rule")

// ApplyOption configures Apply.
type ApplyOption func(*applyOptions)

type applyOptions struct{ force bool }

// Force carries out a plan whose guards fired. It exists on apply and
// deliberately not on plan: plan writes nothing, so silencing its exit 3 would
// only ever mean not reading it. Here the guard stands between a decision and
// two live instances, so the way past has to be explicit and has to be logged.
func Force() ApplyOption {
	return func(o *applyOptions) { o.force = true }
}

// Apply carries out a plan and returns what landed. A failed batch does not
// abort the rule: one folder rejecting a write is no reason to leave the other
// forty untouched, and the returned state records exactly the actions that
// succeeded, so the next pass retries the rest and nothing else.
func Apply(ctx context.Context, writers map[string]Writer, p *Plan, opts ...ApplyOption) (*ApplyResult, error) {
	var o applyOptions
	for _, opt := range opts {
		opt(&o)
	}

	if p.Blocked() && !o.force {
		return nil, fmt.Errorf("%w: %s", ErrBlocked, joinGuards(p.Guards))
	}
	if p.Pair.DryRun {
		// Nothing is written, including the state. Recording a dry run as
		// though it had synced would make the next real run believe both sides
		// already agree.
		return &ApplyResult{DryRun: true}, nil
	}

	res := &ApplyResult{State: cloneState(p.Next)}

	// One resolution per side, not one per folder: the slug is fixed for the
	// whole rule and each lookup is a round trip.
	projects := make(map[Side]string, 2)
	for _, side := range []Side{SideA, SideB} {
		scope := p.Pair.A
		if side == SideB {
			scope = p.Pair.B
		}
		if !hasWork(p.Actions, side) {
			continue
		}
		w, ok := writers[scope.Instance]
		if !ok {
			return nil, fmt.Errorf("no writer for instance %q", scope.Instance)
		}
		id, err := w.ResolveProjectID(ctx, scope.Project, scope.Env)
		if err != nil {
			return nil, fmt.Errorf("side %s (%s): %w", side, scope, err)
		}
		projects[side] = id
	}

	for _, batch := range batches(p.Actions) {
		scope := p.Pair.A
		if batch.side == SideB {
			scope = p.Pair.B
		}
		absPath := config.JoinRelative(scope.Path, batch.relPath)
		w := writers[scope.Instance]

		err := runBatch(ctx, w, batch, projects[batch.side], scope.Env, absPath)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("side %s, %s in %s: %w", batch.side, batch.op, absPath, err))
			res.Failed += len(batch.actions)
			revert(res.State, batch.actions)
			continue
		}

		switch batch.op {
		case OpCreate:
			res.Created += len(batch.actions)
		case OpUpdate:
			res.Updated += len(batch.actions)
		case OpDelete:
			res.Deleted += len(batch.actions)
		}
	}
	return res, nil
}

// runBatch carries out one folder's worth of one operation.
func runBatch(ctx context.Context, w Writer, b batch, projectID, env, absPath string) error {
	switch b.op {
	case OpCreate:
		// Only creates need this. Infisical does not create a destination
		// folder implicitly, and an update or a delete is proof the folder is
		// already there, so calling it on those would be a round trip per
		// folder to learn something the operation itself asserts.
		if err := w.EnsureFolder(ctx, projectID, env, absPath); err != nil {
			return fmt.Errorf("creating the destination folder: %w", err)
		}
		return w.CreateSecrets(ctx, infisical.WriteRequest{
			ProjectID:   projectID,
			Environment: env,
			Path:        absPath,
			Secrets:     payload(b.actions),
		})
	case OpUpdate:
		return w.UpdateSecrets(ctx, infisical.WriteRequest{
			ProjectID:   projectID,
			Environment: env,
			Path:        absPath,
			Secrets:     payload(b.actions),
		})
	case OpDelete:
		keys := make([]string, 0, len(b.actions))
		for _, a := range b.actions {
			keys = append(keys, a.Key)
		}
		return w.DeleteSecrets(ctx, infisical.DeleteRequest{
			ProjectID:   projectID,
			Environment: env,
			Path:        absPath,
			Keys:        keys,
		})
	default:
		// Unreachable: batches only groups the three writing ops.
		return fmt.Errorf("reconcile: %s is not a writing operation", b.op)
	}
}

// payload turns actions into write bodies. This is the only place a value
// leaves an Action, and it is in this package because the field is unexported,
// which is what keeps every other package from being able to print one.
func payload(actions []Action) []infisical.SecretWrite {
	out := make([]infisical.SecretWrite, 0, len(actions))
	for _, a := range actions {
		out = append(out, infisical.SecretWrite{Key: a.Key, Value: a.value, Comment: a.comment})
	}
	return out
}

// revert puts back the state entries of actions that did not land. An entry
// absent before the run is removed rather than left at its planned value:
// "never seen" and "seen and agreed" are different answers to the next pass's
// question, and only the first one is true.
func revert(st *state.RuleState, actions []Action) {
	for _, a := range actions {
		if a.entryKey == "" {
			continue
		}
		if a.hadPrev {
			st.Entries[a.entryKey] = a.prev
			continue
		}
		st.Delete(a.entryKey)
	}
}

// batch is one folder's worth of one operation on one side: the unit the API
// takes and the unit that succeeds or fails together.
type batch struct {
	side    Side
	op      Op
	relPath string
	actions []Action
}

// batches groups the writing actions so that each group is a single request.
// The order is deterministic (side, then folder, then create before update
// before delete) so two runs over the same plan issue the same requests in the
// same order, which is what makes a partial failure reproducible.
func batches(actions []Action) []batch {
	grouped := map[Side]map[string]map[Op][]Action{}
	for _, a := range actions {
		if !a.Op.Writes() {
			continue
		}
		if grouped[a.Side] == nil {
			grouped[a.Side] = map[string]map[Op][]Action{}
		}
		if grouped[a.Side][a.RelPath] == nil {
			grouped[a.Side][a.RelPath] = map[Op][]Action{}
		}
		grouped[a.Side][a.RelPath][a.Op] = append(grouped[a.Side][a.RelPath][a.Op], a)
	}

	var out []batch
	for _, side := range []Side{SideA, SideB} {
		folders := grouped[side]
		paths := make([]string, 0, len(folders))
		for p := range folders {
			paths = append(paths, p)
		}
		sort.Strings(paths)

		for _, p := range paths {
			for _, op := range []Op{OpCreate, OpUpdate, OpDelete} {
				if acts := folders[p][op]; len(acts) > 0 {
					out = append(out, batch{side: side, op: op, relPath: p, actions: acts})
				}
			}
		}
	}
	return out
}

// hasWork reports whether any action writes to the given side.
func hasWork(actions []Action, side Side) bool {
	for _, a := range actions {
		if a.Side == side && a.Op.Writes() {
			return true
		}
	}
	return false
}

func joinGuards(guards []Guard) string {
	parts := make([]string, 0, len(guards))
	for _, g := range guards {
		parts = append(parts, g.String())
	}
	return strings.Join(parts, "; ")
}
