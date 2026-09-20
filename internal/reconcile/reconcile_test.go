package reconcile

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alekc/infisical-mirror/internal/config"
	"github.com/alekc/infisical-mirror/internal/state"
)

const testSalt = "a-salt-long-enough-to-be-accepted"

var (
	past   = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	now    = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	older  = time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	newer  = time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	theKey = state.EntryKey("/", "API_KEY")
)

func testHasher(t *testing.T) *state.Hasher {
	t.Helper()
	h, err := state.NewHasher([]byte(testSalt))
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	return h
}

func testPair(mode config.Mode, conflict config.ConflictPolicy, del config.DeletePolicy) config.Pair {
	return config.Pair{
		Rule:      "test",
		Mode:      mode,
		A:         config.Scope{Instance: "cloud", Project: "p", Env: "prod", Path: "/"},
		B:         config.Scope{Instance: "self", Project: "q", Env: "prod", Path: "/"},
		Recursive: true,
		Conflict:  conflict,
		Delete:    del,
		// The decision rows are about decisions. The guards get their own
		// tests, and a ratio of 1 keeps them out of the way here.
		MaxChangeRatio: 1,
	}
}

// want is one expected action, with the value that would be written.
type want struct {
	op    Op
	side  Side
	value string
}

type row struct {
	name  string
	mode  config.Mode
	confl config.ConflictPolicy
	del   config.DeletePolicy

	a, b     *string
	aAt, bAt time.Time
	// last is the value both sides held at the last successful sync. nil
	// means this pair has never synced this key.
	last *string
	// tomb records the key as deleted everywhere by an earlier run.
	tomb bool

	want      []want
	converged int
	untracked int
}

func TestReconcileDecisionTable(t *testing.T) {
	const (
		bi   = config.ModeBidirectional
		atob = config.ModeAToB
		btoa = config.ModeBToA
	)
	const (
		newest = config.ConflictNewestWins
		aWins  = config.ConflictAWins
		bWins  = config.ConflictBWins
		fail   = config.ConflictFail
	)
	const (
		ignore    = config.DeleteIgnore
		propagate = config.DeletePropagate
	)

	rows := []row{
		{
			name: "bidirectional: identical on both sides",
			mode: bi, confl: newest, del: ignore,
			a: new("v1"), b: new("v1"), last: new("v1"),
			converged: 1,
		},
		{
			name: "bidirectional: changed on a only",
			mode: bi, confl: newest, del: ignore,
			a: new("v2"), b: new("v1"), last: new("v1"),
			want: []want{{OpUpdate, SideB, "v2"}},
		},
		{
			name: "bidirectional: changed on b only",
			mode: bi, confl: newest, del: ignore,
			a: new("v1"), b: new("v2"), last: new("v1"),
			want: []want{{OpUpdate, SideA, "v2"}},
		},
		{
			name: "bidirectional: both changed, conflict fail",
			mode: bi, confl: fail, del: ignore,
			a: new("v2"), b: new("v3"), last: new("v1"),
			want: []want{{OpConflict, "", ""}},
		},
		{
			name: "bidirectional: both changed, a-wins",
			mode: bi, confl: aWins, del: ignore,
			a: new("v2"), b: new("v3"), last: new("v1"),
			want: []want{{OpUpdate, SideB, "v2"}},
		},
		{
			name: "bidirectional: both changed, b-wins",
			mode: bi, confl: bWins, del: ignore,
			a: new("v2"), b: new("v3"), last: new("v1"),
			want: []want{{OpUpdate, SideA, "v3"}},
		},
		{
			name: "bidirectional: both changed, newest-wins picks a",
			mode: bi, confl: newest, del: ignore,
			a: new("v2"), b: new("v3"), aAt: newer, bAt: older, last: new("v1"),
			want: []want{{OpUpdate, SideB, "v2"}},
		},
		{
			name: "bidirectional: both changed, newest-wins picks b",
			mode: bi, confl: newest, del: ignore,
			a: new("v2"), b: new("v3"), aAt: older, bAt: newer, last: new("v1"),
			want: []want{{OpUpdate, SideA, "v3"}},
		},
		{
			name: "bidirectional: both changed, newest-wins cannot break a tie",
			mode: bi, confl: newest, del: ignore,
			a: new("v2"), b: new("v3"), aAt: newer, bAt: newer, last: new("v1"),
			want: []want{{OpConflict, "", ""}},
		},
		{
			name: "bidirectional: differ with no state at all",
			mode: bi, confl: newest, del: ignore,
			a: new("v2"), b: new("v3"), aAt: newer, bAt: older,
			want: []want{{OpUpdate, SideB, "v2"}},
		},
		{
			name: "bidirectional: new on a",
			mode: bi, confl: newest, del: ignore,
			a:    new("v1"),
			want: []want{{OpCreate, SideB, "v1"}},
		},
		{
			name: "bidirectional: new on b",
			mode: bi, confl: newest, del: ignore,
			b:    new("v1"),
			want: []want{{OpCreate, SideA, "v1"}},
		},
		{
			name: "bidirectional: added back after being deleted everywhere",
			mode: bi, confl: newest, del: ignore,
			a: new("v1"), tomb: true,
			want: []want{{OpCreate, SideB, "v1"}},
		},
		{
			name: "bidirectional: deleted on b, delete ignore",
			mode: bi, confl: newest, del: ignore,
			a: new("v1"), last: new("v1"),
			want: []want{{OpDeleteObserved, "", ""}},
		},
		{
			name: "bidirectional: deleted on b, delete propagate",
			mode: bi, confl: newest, del: propagate,
			a: new("v1"), last: new("v1"),
			want: []want{{OpDelete, SideA, ""}},
		},
		{
			name: "bidirectional: deleted on a, delete propagate",
			mode: bi, confl: newest, del: propagate,
			b: new("v1"), last: new("v1"),
			want: []want{{OpDelete, SideB, ""}},
		},
		{
			name: "bidirectional: deleted on b while a was edited, ignore",
			mode: bi, confl: newest, del: ignore,
			a: new("v2"), last: new("v1"),
			want: []want{{OpConflict, "", ""}},
		},
		{
			name: "bidirectional: deleted on b while a was edited, propagate",
			mode: bi, confl: newest, del: propagate,
			a: new("v2"), last: new("v1"),
			want: []want{{OpConflict, "", ""}},
		},
		{
			name: "bidirectional: gone from both sides",
			mode: bi, confl: newest, del: ignore,
			last: new("v1"),
		},
		{
			name: "a-to-b: identical",
			mode: atob, confl: newest, del: ignore,
			a: new("v1"), b: new("v1"), last: new("v1"),
			converged: 1,
		},
		{
			name: "a-to-b: the destination was edited, source wins anyway",
			mode: atob, confl: newest, del: ignore,
			a: new("v1"), b: new("v2"), aAt: older, bAt: newer, last: new("v1"),
			want: []want{{OpUpdate, SideB, "v1"}},
		},
		{
			name: "a-to-b: new on the source",
			mode: atob, confl: newest, del: ignore,
			a:    new("v1"),
			want: []want{{OpCreate, SideB, "v1"}},
		},
		{
			name: "a-to-b: deleted on the destination, restored",
			mode: atob, confl: newest, del: ignore,
			a: new("v1"), last: new("v1"),
			want: []want{{OpCreate, SideB, "v1"}},
		},
		{
			name: "a-to-b: a key the source never had is left alone",
			mode: atob, confl: newest, del: ignore,
			b:         new("v1"),
			untracked: 1,
		},
		{
			name: "a-to-b: deleted on the source, delete ignore",
			mode: atob, confl: newest, del: ignore,
			b: new("v1"), last: new("v1"),
			want: []want{{OpDeleteObserved, "", ""}},
		},
		{
			name: "a-to-b: deleted on the source, delete propagate",
			mode: atob, confl: newest, del: propagate,
			b: new("v1"), last: new("v1"),
			want: []want{{OpDelete, SideB, ""}},
		},
		{
			name: "b-to-a: new on the source",
			mode: btoa, confl: newest, del: ignore,
			b:    new("v1"),
			want: []want{{OpCreate, SideA, "v1"}},
		},
		{
			name: "b-to-a: a key the source never had is left alone",
			mode: btoa, confl: newest, del: ignore,
			a:         new("v1"),
			untracked: 1,
		},
		{
			name: "b-to-a: the destination was edited, source wins anyway",
			mode: btoa, confl: newest, del: ignore,
			a: new("v2"), b: new("v1"), last: new("v1"),
			want: []want{{OpUpdate, SideA, "v1"}},
		},
		{
			name: "b-to-a: deleted on the source, delete propagate",
			mode: btoa, confl: newest, del: propagate,
			a: new("v1"), last: new("v1"),
			want: []want{{OpDelete, SideA, ""}},
		},
	}

	h := testHasher(t)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			pair := testPair(r.mode, r.confl, r.del)
			st := state.NewRuleState()
			if r.last != nil {
				hv := h.Hash(theKey, *r.last, "")
				st.Put(theKey, hv, hv, past)
			}
			if r.tomb {
				st.Tombstone(theKey, past)
			}

			out, err := Reconcile(pair, side(r.a, r.aAt), side(r.b, r.bAt), st, h, now)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			if len(out.Actions) != len(r.want) {
				t.Fatalf("got %d action(s) %v, want %d", len(out.Actions), out.Actions, len(r.want))
			}
			for i, w := range r.want {
				got := out.Actions[i]
				if got.Op != w.op || got.Side != w.side {
					t.Errorf("action %d = %s %s, want %s %s", i, got.Op, got.Side, w.op, w.side)
				}
				if got.value != w.value {
					t.Errorf("action %d writes %q, want %q", i, got.value, w.value)
				}
				if got.Reason == "" {
					t.Errorf("action %d has no reason", i)
				}
			}
			if out.Converged != r.converged {
				t.Errorf("converged = %d, want %d", out.Converged, r.converged)
			}
			if out.Untracked != r.untracked {
				t.Errorf("untracked = %d, want %d", out.Untracked, r.untracked)
			}
			// Guards are deliberately not asserted here. These rows carry one
			// key, so any row where that key is deleted on a side leaves that
			// side empty and legitimately trips the empty-side guard.
			// TestReconcileGuards covers them on fixtures built for it.
		})
	}
}

// side builds a one-key snapshot, or an empty one when the value is absent.
func side(v *string, at time.Time) Snapshot {
	if v == nil {
		return Snapshot{}
	}
	return Snapshot{theKey: {Value: *v, UpdatedAt: at}}
}

func TestReconcileComparesComments(t *testing.T) {
	h := testHasher(t)
	pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeleteIgnore)

	a := Snapshot{theKey: {Value: "v1", Comment: "rotated 2026-09"}}
	b := Snapshot{theKey: {Value: "v1"}}
	st := state.NewRuleState()
	hv := h.Hash(theKey, "v1", "")
	st.Put(theKey, hv, hv, past)

	out, err := Reconcile(pair, a, b, st, h, now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(out.Actions) != 1 || out.Actions[0].Op != OpUpdate || out.Actions[0].Side != SideB {
		t.Fatalf("got %v, want one update of side b", out.Actions)
	}
	if out.Actions[0].comment != "rotated 2026-09" {
		t.Errorf("comment = %q, want it carried to the far side", out.Actions[0].comment)
	}
	// Five of the six updates in the first live plan were comment-only, and
	// an update line that does not say so reads as a changed secret.
	if got := out.Actions[0].Reason; !strings.HasPrefix(got, "comment ") {
		t.Errorf("reason = %q, want it to say only the comment differs", got)
	}
}

func TestReconcileNamesWhatDiffers(t *testing.T) {
	cases := map[string]struct{ a, b Secret }{
		"value":             {Secret{Value: "v1"}, Secret{Value: "v2"}},
		"comment":           {Secret{Value: "v1", Comment: "c1"}, Secret{Value: "v1"}},
		"value and comment": {Secret{Value: "v1", Comment: "c1"}, Secret{Value: "v2", Comment: "c2"}},
	}
	for want, c := range cases {
		if got := differs(c.a, c.b); got != want {
			t.Errorf("differs() = %q, want %q", got, want)
		}
	}
}

func TestReconcileNextState(t *testing.T) {
	h := testHasher(t)
	pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeletePropagate)

	deleted := state.EntryKey("/", "GONE")
	conflicted := state.EntryKey("/", "BOTH")
	st := state.NewRuleState()
	for _, k := range []string{theKey, deleted, conflicted} {
		hv := h.Hash(k, "v1", "")
		st.Put(k, hv, hv, past)
	}

	a := Snapshot{
		theKey:     {Value: "v2"},
		conflicted: {Value: "v2", UpdatedAt: newer},
	}
	b := Snapshot{
		theKey:     {Value: "v1"},
		conflicted: {Value: "v3", UpdatedAt: newer},
	}

	out, err := Reconcile(pair, a, b, st, h, now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// The write: both sides would hold a's value, so both hashes record it.
	entry, ok := out.Next.Get(theKey)
	if !ok || entry.HashA != h.Hash(theKey, "v2", "") || entry.HashA != entry.HashB {
		t.Errorf("written key recorded as %+v, want both sides at the new hash", entry)
	}
	// The propagated delete: tombstoned, so the next run does not read the
	// key coming back as a brand new secret on one side.
	if entry, ok := out.Next.Get(deleted); !ok || !entry.Tombstone {
		t.Errorf("deleted key recorded as %+v, want a tombstone", entry)
	}
	// The conflict: nothing was written, so nothing about the last sync
	// changed, and the same conflict must be reported again next run.
	if entry, ok := out.Next.Get(conflicted); !ok || entry.HashA != h.Hash(conflicted, "v1", "") {
		t.Errorf("conflicted key recorded as %+v, want the last synced hash untouched", entry)
	}

	// State is an input, not a scratchpad: the caller's copy is unchanged
	// until it decides to save the new one.
	if before, _ := st.Get(deleted); before.Tombstone {
		t.Error("Reconcile mutated the state it was given")
	}
}

func TestReconcileLeavesOutOfScopeEntriesAlone(t *testing.T) {
	h := testHasher(t)
	pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeletePropagate)
	pair.Exclude = []string{"/scratch/**"}

	stale := state.EntryKey("/scratch", "OLD")
	st := state.NewRuleState()
	hv := h.Hash(stale, "v1", "")
	st.Put(stale, hv, hv, past)

	out, err := Reconcile(pair, Snapshot{}, Snapshot{}, st, h, now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(out.Actions) != 0 {
		t.Errorf("got %v, want a narrowed rule to leave its old folders alone", out.Actions)
	}
	if out.OutOfScope != 1 {
		t.Errorf("outOfScope = %d, want 1", out.OutOfScope)
	}
	if entry, ok := out.Next.Get(stale); !ok || entry.Tombstone {
		t.Errorf("entry recorded as %+v, want it carried forward untouched", entry)
	}
	if len(out.Guards) != 0 {
		t.Errorf("unexpected guards: %v", out.Guards)
	}
}

func TestReconcileSortsActions(t *testing.T) {
	h := testHasher(t)
	pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeleteIgnore)

	a := Snapshot{
		state.EntryKey("/sub", "B_KEY"): {Value: "v"},
		state.EntryKey("/", "Z_KEY"):    {Value: "v"},
		state.EntryKey("/", "A_KEY"):    {Value: "v"},
		state.EntryKey("/sub", "A_KEY"): {Value: "v"},
	}

	out, err := Reconcile(pair, a, Snapshot{}, state.NewRuleState(), h, now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got []string
	for _, act := range out.Actions {
		got = append(got, act.RelPath+":"+act.Key)
	}
	wantOrder := []string{"/:A_KEY", "/:Z_KEY", "/sub:A_KEY", "/sub:B_KEY"}
	if strings.Join(got, " ") != strings.Join(wantOrder, " ") {
		t.Errorf("order = %v, want %v", got, wantOrder)
	}
}

func TestActionStringHidesTheValue(t *testing.T) {
	a := Action{
		Op:      OpUpdate,
		Side:    SideB,
		RelPath: "/",
		Key:     "API_KEY",
		Reason:  "changed on a since the last sync",
		value:   "super-secret-value",
		comment: "and the comment",
	}
	for _, s := range []string{a.String(), fmtSprint(a), fmtSprintPlus(a)} {
		if strings.Contains(s, "super-secret-value") || strings.Contains(s, "and the comment") {
			t.Fatalf("rendering an action leaked its value: %s", s)
		}
	}
	if !strings.Contains(a.String(), "API_KEY") {
		t.Errorf("String() = %q, want it to name the key", a.String())
	}
}

// fmt reaches unexported fields through reflection, so these two are the
// routes a value would take into a log line if String() did not exist.
func fmtSprint(a Action) string     { return fmt.Sprint(a) }
func fmtSprintPlus(a Action) string { return fmt.Sprintf("%+v", a) }

// A zero timestamp is not an old timestamp. Go's zero time is before every real
// one, so under newest-wins a side whose updatedAt was absent or unparseable
// loses every conflict in silence. On an instance whose timestamp format moved,
// that is the whole estate overwritten and reported as routine updates.
func TestNewestWinsRefusesToChooseWithoutATimestamp(t *testing.T) {
	h := testHasher(t)
	pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeleteIgnore)

	cases := map[string]struct {
		aAt, bAt time.Time
		names    string
	}{
		"side a has none":    {time.Time{}, newer, "side a"},
		"side b has none":    {newer, time.Time{}, "side b"},
		"neither side has a": {time.Time{}, time.Time{}, "neither side"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := Snapshot{theKey: {Value: "v-a", UpdatedAt: tc.aAt}}
			b := Snapshot{theKey: {Value: "v-b", UpdatedAt: tc.bAt}}

			out, err := Reconcile(pair, a, b, state.NewRuleState(), h, now)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if out.Conflicts() != 1 || len(out.Actions) != 1 {
				t.Fatalf("got %v, want exactly one conflict and no write", out.Actions)
			}
			if out.Actions[0].Op != OpConflict {
				t.Fatalf("action = %s, want a conflict", out.Actions[0].Op)
			}
			// The reason has to say where to look: the operator's next move is
			// to go and see why that instance sends no update time.
			got := out.Actions[0].Reason
			if !strings.Contains(got, "no usable update time") || !strings.Contains(got, tc.names) {
				t.Errorf("reason = %q, want it to name %s", got, tc.names)
			}
		})
	}

	// Two real but identical timestamps is the other tie, and is also refused:
	// picking a side there is a coin toss that overwrites a real secret.
	a := Snapshot{theKey: {Value: "v-a", UpdatedAt: newer}}
	b := Snapshot{theKey: {Value: "v-b", UpdatedAt: newer}}
	out, err := Reconcile(pair, a, b, state.NewRuleState(), h, now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if out.Conflicts() != 1 || len(out.Actions) != 1 || out.Actions[0].Op != OpConflict {
		t.Errorf("an exact tie produced %v, want one conflict and no write", out.Actions)
	}
}

// A key that state knows about and neither side has any more is retired: the
// state entry becomes a tombstone. That is a change to the file, so it has to
// be counted and printed, or the plan's stated effect is not its whole effect.
func TestRetiredKeysAreCountedAndTombstoned(t *testing.T) {
	h := testHasher(t)
	pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeleteIgnore)

	gone := state.EntryKey("/", "GONE")
	already := state.EntryKey("/", "ALREADY")
	st := state.NewRuleState()
	hv := h.Hash(gone, "v1", "")
	st.Put(gone, hv, hv, past)
	st.Tombstone(already, past)

	out, err := Reconcile(pair, Snapshot{}, Snapshot{}, st, h, now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(out.Actions) != 0 {
		t.Fatalf("got %v, want nothing written for a key neither side has", out.Actions)
	}
	// One, not two: a key that was already a tombstone is not retired again,
	// or every run would report the same retirement forever.
	if out.Retired != 1 {
		t.Errorf("retired = %d, want 1", out.Retired)
	}
	if entry, ok := out.Next.Get(gone); !ok || !entry.Tombstone {
		t.Errorf("the vanished key is recorded as %+v, want a tombstone", entry)
	}
}

// Tracked counts the live entries in the whole rule state; InScope counts the
// ones this rule still selects. They differ once a rule is narrowed, and the
// change-ratio guard divides by the second: dividing by the first makes a rule
// covering three keys look safe because the file remembers three hundred.
func TestInScopeExcludesWhatTheRuleNoLongerSelects(t *testing.T) {
	h := testHasher(t)
	pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeletePropagate)
	pair.Exclude = []string{"/scratch/**"}

	st := state.NewRuleState()
	for i := range 10 {
		k := state.EntryKey("/scratch", fmt.Sprintf("OLD_%02d", i))
		hv := h.Hash(k, "v1", "")
		st.Put(k, hv, hv, past)
	}
	live := state.EntryKey("/", "LIVE")
	hv := h.Hash(live, "v1", "")
	st.Put(live, hv, hv, past)

	a := Snapshot{live: {Value: "v1"}}
	out, err := Reconcile(pair, a, Snapshot{}, st, h, now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Tracked lives on Plan, computed from the state store; InScope is the
	// reconciler's own view of how much of that this rule still selects.
	if got := st.Tracked(); got != 11 {
		t.Errorf("the state file holds %d live entries, want 11", got)
	}
	if out.InScope != 1 {
		t.Errorf("inScope = %d, want only the entry this rule still selects", out.InScope)
	}
}
