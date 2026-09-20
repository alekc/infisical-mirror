package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/alekc/infisical-mirror/internal/config"
	"github.com/alekc/infisical-mirror/internal/infisical"
	"github.com/alekc/infisical-mirror/internal/state"
)

// fakeWriter records what it was asked to do and can be told to fail a
// specific folder, which is what the partial-failure tests turn on.
type fakeWriter struct {
	created  map[string][]infisical.SecretWrite
	updated  map[string][]infisical.SecretWrite
	deleted  map[string][]string
	folders  []string
	resolved int

	// failOn makes any write touching this absolute path fail.
	failOn string
	// failResolve makes the project lookup fail.
	failResolve bool
}

func newFakeWriter() *fakeWriter {
	return &fakeWriter{
		created: map[string][]infisical.SecretWrite{},
		updated: map[string][]infisical.SecretWrite{},
		deleted: map[string][]string{},
	}
}

func (f *fakeWriter) ResolveProjectID(_ context.Context, slug string, _ ...string) (string, error) {
	f.resolved++
	if f.failResolve {
		return "", errors.New("no such project")
	}
	return "id-" + slug, nil
}

func (f *fakeWriter) EnsureFolder(_ context.Context, _, _, absPath string) error {
	if absPath == f.failOn {
		return errors.New("folder refused")
	}
	f.folders = append(f.folders, absPath)
	return nil
}

func (f *fakeWriter) CreateSecrets(_ context.Context, req infisical.WriteRequest) error {
	if req.Path == f.failOn {
		return errors.New("create refused")
	}
	f.created[req.Path] = append(f.created[req.Path], req.Secrets...)
	return nil
}

func (f *fakeWriter) UpdateSecrets(_ context.Context, req infisical.WriteRequest) error {
	if req.Path == f.failOn {
		return errors.New("update refused")
	}
	f.updated[req.Path] = append(f.updated[req.Path], req.Secrets...)
	return nil
}

func (f *fakeWriter) DeleteSecrets(_ context.Context, req infisical.DeleteRequest) error {
	if req.Path == f.failOn {
		return errors.New("delete refused")
	}
	f.deleted[req.Path] = append(f.deleted[req.Path], req.Keys...)
	return nil
}

// planFrom reconciles two snapshots and wraps the outcome as a Plan, which is
// what Apply takes.
func planFrom(t *testing.T, pair config.Pair, a, b Snapshot, st *state.RuleState) *Plan {
	t.Helper()
	out, err := Reconcile(pair, a, b, st, testHasher(t), now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return &Plan{Outcome: out, Pair: pair, Scope: pair.StateScope(), Tracked: st.Tracked()}
}

func writers(w *fakeWriter) map[string]Writer {
	return map[string]Writer{"cloud": w, "self": w}
}

func TestApplyWritesEachFolderAsOneBatch(t *testing.T) {
	pair := testPair(config.ModeAToB, config.ConflictNewestWins, config.DeleteIgnore)
	a := Snapshot{
		state.EntryKey("/", "ROOT"):       {Value: "r"},
		state.EntryKey("/db", "USER"):     {Value: "u"},
		state.EntryKey("/db", "PASSWORD"): {Value: "p"},
	}
	plan := planFrom(t, pair, a, Snapshot{}, state.NewRuleState())

	w := newFakeWriter()
	res, err := Apply(t.Context(), writers(w), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if res.Created != 3 || res.Failed != 0 {
		t.Fatalf("created %d, failed %d; want 3 and 0", res.Created, res.Failed)
	}
	// Two folders, so two create calls, not three: batching per folder is the
	// difference between three round trips and two, and at estate scale
	// between hundreds and dozens.
	if got := len(w.created); got != 2 {
		t.Fatalf("wrote into %d folder(s), want 2: %v", got, w.created)
	}
	if got := len(w.created["/db"]); got != 2 {
		t.Fatalf("/db got %d secret(s) in one call, want 2", got)
	}
	// Both destination folders are ensured before their creates.
	if len(w.folders) != 2 {
		t.Fatalf("ensured %d folder(s), want 2: %v", len(w.folders), w.folders)
	}
	// One resolution for the whole rule, not one per folder.
	if w.resolved != 1 {
		t.Fatalf("resolved the project %d time(s), want 1", w.resolved)
	}
}

// TestApplyCarriesTheValueThroughToTheWrite is the one test that checks the
// payload reaches the API unchanged. Everything else about Action deliberately
// hides the value, so without this the plumbing could drop it and every other
// test would still pass.
func TestApplyCarriesTheValueThroughToTheWrite(t *testing.T) {
	pair := testPair(config.ModeAToB, config.ConflictNewestWins, config.DeleteIgnore)
	a := Snapshot{state.EntryKey("/", "API_KEY"): {Value: "s3cret", Comment: "a note"}}
	plan := planFrom(t, pair, a, Snapshot{}, state.NewRuleState())

	w := newFakeWriter()
	if _, err := Apply(t.Context(), writers(w), plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := w.created["/"]
	if len(got) != 1 {
		t.Fatalf("got %d write(s), want 1", len(got))
	}
	if got[0].Key != "API_KEY" || got[0].Value != "s3cret" || got[0].Comment != "a note" {
		t.Fatalf("wrote %+v, want key API_KEY with its value and comment", got[0])
	}
}

// TestAFailedBatchLeavesItsKeysUnsynced is the whole reason Apply exists rather
// than the caller persisting Outcome.Next, which describes the world where
// every action succeeded. Persisting it after a partial failure records two
// sides as agreeing on a secret that was never written.
func TestAFailedBatchLeavesItsKeysUnsynced(t *testing.T) {
	pair := testPair(config.ModeAToB, config.ConflictNewestWins, config.DeleteIgnore)
	a := Snapshot{
		state.EntryKey("/ok", "FINE"):     {Value: "v"},
		state.EntryKey("/bad", "BROKEN"):  {Value: "v"},
		state.EntryKey("/bad", "BROKEN2"): {Value: "v"},
	}
	plan := planFrom(t, pair, a, Snapshot{}, state.NewRuleState())

	w := newFakeWriter()
	w.failOn = "/bad"
	res, err := Apply(t.Context(), writers(w), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if res.Created != 1 {
		t.Fatalf("created %d, want 1", res.Created)
	}
	if res.Failed != 2 {
		t.Fatalf("failed %d, want 2", res.Failed)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("got %d error(s), want one per failed batch", len(res.Errors))
	}

	// The successful key is recorded; neither failed key is.
	if _, ok := res.State.Get(state.EntryKey("/ok", "FINE")); !ok {
		t.Error("the key that was written is not in the state, so the next pass will write it again")
	}
	for _, key := range []string{state.EntryKey("/bad", "BROKEN"), state.EntryKey("/bad", "BROKEN2")} {
		if _, ok := res.State.Get(key); ok {
			t.Errorf("%s was recorded as synced but its write failed, so the next pass will see no drift and never retry it", key)
		}
	}
}

// TestAFailedUpdateRestoresThePreviousEntry covers the other half: a key that
// was already tracked must go back to the hashes it had, not be dropped. Losing
// the entry would make the next pass unable to tell which side changed.
func TestAFailedUpdateRestoresThePreviousEntry(t *testing.T) {
	pair := testPair(config.ModeAToB, config.ConflictNewestWins, config.DeleteIgnore)
	h := testHasher(t)
	key := state.EntryKey("/", "API_KEY")

	st := state.NewRuleState()
	oldHash := h.Hash(key, "old", "")
	st.Put(key, oldHash, oldHash, past)

	a := Snapshot{key: {Value: "new"}}
	b := Snapshot{key: {Value: "old"}}
	plan := planFrom(t, pair, a, b, st)

	w := newFakeWriter()
	w.failOn = "/"
	res, err := Apply(t.Context(), writers(w), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Failed != 1 {
		t.Fatalf("failed %d, want 1", res.Failed)
	}

	entry, ok := res.State.Get(key)
	if !ok {
		t.Fatal("the entry was dropped; without it the next pass cannot tell which side changed")
	}
	if entry.HashA != oldHash || entry.HashB != oldHash {
		t.Fatalf("entry is %+v, want both hashes back at the pre-run %s", entry, state.Short(oldHash))
	}
	if !entry.SyncedAt.Equal(past) {
		t.Fatalf("syncedAt is %s, want the pre-run %s: a failed write did not sync anything", entry.SyncedAt, past)
	}
}

func TestApplyRefusesABlockedPlan(t *testing.T) {
	pair := testPair(config.ModeBidirectional, config.ConflictAWins, config.DeletePropagate)
	pair.MaxChangeRatio = 0.25

	h := testHasher(t)
	st := state.NewRuleState()
	a := Snapshot{}
	for i := range 8 {
		key := state.EntryKey("/", fmt.Sprintf("K%d", i))
		hash := h.Hash(key, "old", "")
		st.Put(key, hash, hash, past)
		a[key] = Secret{Value: "new"}
	}
	b := Snapshot{}
	for k := range a {
		b[k] = Secret{Value: "old"}
	}

	plan := planFrom(t, pair, a, b, st)
	if !plan.Blocked() {
		t.Fatal("the fixture did not trip a guard, so this test proves nothing")
	}

	w := newFakeWriter()
	_, err := Apply(t.Context(), writers(w), plan)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if len(w.created) != 0 || len(w.updated) != 0 || len(w.deleted) != 0 {
		t.Fatal("a refused plan wrote something")
	}
	// The guard's own words have to survive into the error, or an operator has
	// to re-run plan to find out what refused.
	if !strings.Contains(err.Error(), "max-change-ratio") {
		t.Errorf("error %q does not name the guard that fired", err)
	}

	// The same plan, forced, goes through.
	w2 := newFakeWriter()
	res, err := Apply(t.Context(), writers(w2), plan, Force())
	if err != nil {
		t.Fatalf("Apply with Force: %v", err)
	}
	if res.Updated != 8 {
		t.Fatalf("forced apply updated %d, want 8", res.Updated)
	}
}

func TestApplyOnADryRunRuleWritesNothingAtAll(t *testing.T) {
	pair := testPair(config.ModeAToB, config.ConflictNewestWins, config.DeleteIgnore)
	pair.DryRun = true

	a := Snapshot{state.EntryKey("/", "API_KEY"): {Value: "v"}}
	plan := planFrom(t, pair, a, Snapshot{}, state.NewRuleState())

	w := newFakeWriter()
	res, err := Apply(t.Context(), writers(w), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !res.DryRun || res.Wrote() != 0 {
		t.Fatalf("result is %+v, want a dry run that wrote nothing", res)
	}
	if w.resolved != 0 || len(w.created) != 0 {
		t.Fatal("a dry run reached the instance")
	}
	// The state in particular: recording a dry run as though it had synced
	// would make the next real run believe both sides already agree.
	if res.State != nil {
		t.Fatal("a dry run produced a state to persist, which would record a sync that never happened")
	}
}

func TestApplyDeletesThroughTheDeleteEndpoint(t *testing.T) {
	pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeletePropagate)
	h := testHasher(t)
	key := state.EntryKey("/", "GONE")

	st := state.NewRuleState()
	hash := h.Hash(key, "v", "")
	st.Put(key, hash, hash, past)

	// A second key, converged and present on both sides, so that side a is not
	// wholly empty. An empty side is its own guard, and it fires first: the
	// fixture has to be a deletion rather than a side that vanished.
	keep := state.EntryKey("/", "KEPT")
	keepHash := h.Hash(keep, "k", "")
	st.Put(keep, keepHash, keepHash, past)

	// GONE is present on b, gone from a: with propagate, b's copy goes too.
	plan := planFrom(t, pair,
		Snapshot{keep: {Value: "k"}},
		Snapshot{key: {Value: "v"}, keep: {Value: "k"}},
		st)

	w := newFakeWriter()
	res, err := Apply(t.Context(), writers(w), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("deleted %d, want 1", res.Deleted)
	}
	if got := w.deleted["/"]; len(got) != 1 || got[0] != "GONE" {
		t.Fatalf("deleted %v, want [GONE]", got)
	}
	// A delete does not ensure its folder: the folder demonstrably exists,
	// since something is being removed from it.
	if len(w.folders) != 0 {
		t.Fatalf("ensured %v before a delete, which is a round trip for nothing", w.folders)
	}
}

func TestApplyOrdersBatchesDeterministically(t *testing.T) {
	pair := testPair(config.ModeAToB, config.ConflictNewestWins, config.DeleteIgnore)
	a := Snapshot{
		state.EntryKey("/z", "K"): {Value: "v"},
		state.EntryKey("/a", "K"): {Value: "v"},
		state.EntryKey("/m", "K"): {Value: "v"},
	}
	plan := planFrom(t, pair, a, Snapshot{}, state.NewRuleState())

	// Run it repeatedly: map iteration order is randomised per run, so an
	// ordering that depends on it fails this within a few attempts rather than
	// once in production.
	for range 20 {
		w := newFakeWriter()
		if _, err := Apply(t.Context(), writers(w), plan); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		want := []string{"/a", "/m", "/z"}
		if len(w.folders) != len(want) {
			t.Fatalf("ensured %v, want %v", w.folders, want)
		}
		for i := range want {
			if w.folders[i] != want[i] {
				t.Fatalf("ensured %v, want %v: a partial failure has to be reproducible", w.folders, want)
			}
		}
	}
}

func TestApplyResolvesOnlyTheSidesItWritesTo(t *testing.T) {
	// A one-way rule writes only to b, so a's project never needs resolving.
	pair := testPair(config.ModeAToB, config.ConflictNewestWins, config.DeleteIgnore)
	a := Snapshot{state.EntryKey("/", "K"): {Value: "v"}}
	plan := planFrom(t, pair, a, Snapshot{}, state.NewRuleState())

	w := newFakeWriter()
	if _, err := Apply(t.Context(), writers(w), plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if w.resolved != 1 {
		t.Fatalf("resolved %d project(s), want 1: only side b is written to", w.resolved)
	}
}

func TestApplyStopsWhenTheProjectCannotBeResolved(t *testing.T) {
	pair := testPair(config.ModeAToB, config.ConflictNewestWins, config.DeleteIgnore)
	a := Snapshot{state.EntryKey("/", "K"): {Value: "v"}}
	plan := planFrom(t, pair, a, Snapshot{}, state.NewRuleState())

	w := newFakeWriter()
	w.failResolve = true
	// Unresolvable means every path is wrong, not that one folder is: writing
	// anything under a guessed project id is worse than writing nothing.
	if _, err := Apply(t.Context(), writers(w), plan); err == nil {
		t.Fatal("Apply proceeded without a project id")
	}
	if len(w.created) != 0 {
		t.Fatal("something was written under an unresolved project")
	}
}

func TestJoinRelativeRoundTripsRelativeTo(t *testing.T) {
	// The two are inverses, and getting the direction wrong writes a secret
	// into the wrong folder. A round trip is what actually holds them together.
	for _, tc := range []struct{ root, abs string }{
		{"/", "/"},
		{"/", "/apps"},
		{"/", "/apps/web"},
		{"/mirror", "/mirror"},
		{"/mirror", "/mirror/db"},
		{"/mirror", "/mirror/db/creds"},
	} {
		rel, ok := config.RelativeTo(tc.abs, tc.root)
		if !ok {
			t.Fatalf("RelativeTo(%q, %q) reported the path as outside the root", tc.abs, tc.root)
		}
		if back := config.JoinRelative(tc.root, rel); back != tc.abs {
			t.Errorf("JoinRelative(%q, %q) = %q, want %q", tc.root, rel, back, tc.abs)
		}
	}
}
