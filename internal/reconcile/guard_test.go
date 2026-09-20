package reconcile

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alekc/infisical-mirror/internal/config"
	"github.com/alekc/infisical-mirror/internal/state"
)

// keys builds n snapshot keys with the given value, all in the root folder.
func keys(n int, value string) Snapshot {
	snap := make(Snapshot, n)
	for i := range n {
		snap[state.EntryKey("/", fmt.Sprintf("KEY_%02d", i))] = Secret{Value: value}
	}
	return snap
}

// synced records every key in the snapshot as last synced at that value.
func synced(t *testing.T, snap Snapshot, h *state.Hasher) *state.RuleState {
	t.Helper()
	st := state.NewRuleState()
	for k, s := range snap {
		hv := h.Hash(k, s.Value, s.Comment)
		st.Put(k, hv, hv, past)
	}
	return st
}

func guardNames(out *Outcome) []string {
	var names []string
	for _, g := range out.Guards {
		names = append(names, g.Name)
	}
	return names
}

func TestReconcileGuards(t *testing.T) {
	h := testHasher(t)

	t.Run("a side that reads empty with tracked keys is refused", func(t *testing.T) {
		pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeletePropagate)
		a := keys(10, "v1")
		st := synced(t, a, h)

		out, err := Reconcile(pair, a, Snapshot{}, st, h, now)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if got := guardNames(out); len(got) != 1 || got[0] != "empty-side" {
			t.Fatalf("guards = %v, want one empty-side", out.Guards)
		}
		if !strings.Contains(out.Guards[0].Detail, "side b") {
			t.Errorf("detail = %q, want it to name side b", out.Guards[0].Detail)
		}
		if !out.Blocked() {
			t.Error("Blocked() = false, want true")
		}
	})

	t.Run("an empty side on a first run is not refused", func(t *testing.T) {
		pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeleteIgnore)

		out, err := Reconcile(pair, keys(10, "v1"), Snapshot{}, state.NewRuleState(), h, now)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if out.Blocked() {
			t.Fatalf("guards = %v, want none: with no state there is nothing to have lost", out.Guards)
		}
		if out.Writes() != 10 {
			t.Errorf("writes = %d, want 10 creates", out.Writes())
		}
	})

	t.Run("creating is not destructive, however much of it there is", func(t *testing.T) {
		pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeleteIgnore)
		pair.MaxChangeRatio = 0.25

		// Ten tracked keys already agreeing, ten more only on side a.
		agreed := keys(10, "v1")
		st := synced(t, agreed, h)
		a := keys(10, "v1")
		for k, s := range moreKeys(10, "v2") {
			a[k] = s
		}

		out, err := Reconcile(pair, a, agreed, st, h, now)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if out.Blocked() {
			t.Fatalf("guards = %v, want none", out.Guards)
		}
		if out.Writes() != 10 || out.Destructive() != 0 {
			t.Errorf("writes = %d, destructive = %d, want 10 and 0", out.Writes(), out.Destructive())
		}
	})

	t.Run("overwriting too much of a tracked scope is refused", func(t *testing.T) {
		pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeleteIgnore)
		pair.MaxChangeRatio = 0.25

		a := keys(10, "v1")
		st := synced(t, a, h)
		b := keys(10, "v1")
		// Four of the ten changed on b since the last sync.
		for i := range 4 {
			b[state.EntryKey("/", fmt.Sprintf("KEY_%02d", i))] = Secret{Value: "v2"}
		}

		out, err := Reconcile(pair, a, b, st, h, now)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if got := guardNames(out); len(got) != 1 || got[0] != "max-change-ratio" {
			t.Fatalf("guards = %v, want one max-change-ratio", out.Guards)
		}
		if !strings.Contains(out.Guards[0].Detail, "4 of 10") {
			t.Errorf("detail = %q, want it to quote the count", out.Guards[0].Detail)
		}
	})

	t.Run("overwriting a little of a tracked scope is allowed", func(t *testing.T) {
		pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeleteIgnore)
		pair.MaxChangeRatio = 0.25

		a := keys(10, "v1")
		st := synced(t, a, h)
		b := keys(10, "v1")
		b[state.EntryKey("/", "KEY_00")] = Secret{Value: "v2"}

		out, err := Reconcile(pair, a, b, st, h, now)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if out.Blocked() {
			t.Fatalf("guards = %v, want none at 1 of 10", out.Guards)
		}
	})

	t.Run("propagated deletions count towards the ratio", func(t *testing.T) {
		pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeletePropagate)
		pair.MaxChangeRatio = 0.25

		a := keys(10, "v1")
		st := synced(t, a, h)
		b := keys(10, "v1")
		for i := range 4 {
			delete(b, state.EntryKey("/", fmt.Sprintf("KEY_%02d", i)))
		}

		out, err := Reconcile(pair, a, b, st, h, now)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if got := guardNames(out); len(got) != 1 || got[0] != "max-change-ratio" {
			t.Fatalf("guards = %v, want one max-change-ratio", out.Guards)
		}
	})

	t.Run("a first run against two populated sides is measured against both", func(t *testing.T) {
		// The dangerous first run: no state, so nothing says which side moved,
		// and the conflict policy would overwrite whatever it picked. With no
		// tracked keys the keys in play are the denominator, or there would be
		// no ratio to measure this against at all.
		pair := testPair(config.ModeBidirectional, config.ConflictAWins, config.DeleteIgnore)
		pair.MaxChangeRatio = 0.25

		a := keys(10, "v1")
		b := keys(10, "v1")
		for i := range 4 {
			b[state.EntryKey("/", fmt.Sprintf("KEY_%02d", i))] = Secret{Value: "v2"}
		}

		out, err := Reconcile(pair, a, b, state.NewRuleState(), h, now)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if got := guardNames(out); len(got) != 1 || got[0] != "max-change-ratio" {
			t.Fatalf("guards = %v, want one max-change-ratio", out.Guards)
		}
		if !strings.Contains(out.Guards[0].Detail, "4 of 10") {
			t.Errorf("detail = %q, want the keys in play as the denominator", out.Guards[0].Detail)
		}
	})
}

// moreKeys builds keys that do not collide with keys().
func moreKeys(n int, value string) Snapshot {
	snap := make(Snapshot, n)
	for i := range n {
		snap[state.EntryKey("/", fmt.Sprintf("NEW_%02d", i))] = Secret{Value: value}
	}
	return snap
}

// A ratio alone cannot tell a small rule from a runaway one: rotating one
// secret in a folder of three is 33%, over any sane limit. Without a floor the
// guard fires on the most ordinary operation there is, and operators stop that
// by setting maxChangeRatio to 1, which disables it on the rules it was for.
func TestChangeRatioGuardHasAFloor(t *testing.T) {
	h := testHasher(t)

	// The floor is necessary, not sufficient: a run still has to be over the
	// ratio as well. So the rows split three ways, and the first group is what
	// the floor is for: over the 25% ratio, and allowed anyway.
	tests := map[string]struct {
		tracked, changed int
		wantGuard        bool
	}{
		"1 of 3 is over the ratio, under the floor":   {3, 1, false},
		"2 of 3 is over the ratio, under the floor":   {3, 2, false},
		"one key rotated on its own":                  {1, 1, false},
		"3 of 3 clears the floor and the ratio":       {3, 3, true},
		"6 of 20 clears the floor and the ratio":      {20, 6, true},
		"the whole of a big rule":                     {50, 50, true},
		"3 of 20 clears the floor, under the ratio":   {20, 3, false},
		"2 of 100 is under both":                      {100, 2, false},
		"10 of 100 clears the floor, under the ratio": {100, 10, false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeleteIgnore)
			pair.MaxChangeRatio = 0.25

			a := keys(tc.tracked, "v1")
			st := synced(t, a, h)
			b := keys(tc.tracked, "v1")
			for i := range tc.changed {
				b[state.EntryKey("/", fmt.Sprintf("KEY_%02d", i))] = Secret{Value: "v2"}
			}

			out, err := Reconcile(pair, a, b, st, h, now)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if got := out.Destructive(); got != tc.changed {
				t.Fatalf("destructive = %d, want %d: the fixture is not testing what it says", got, tc.changed)
			}

			blocked := len(guardNames(out)) > 0
			if blocked != tc.wantGuard {
				t.Errorf("guards = %v, want a guard: %v (%d of %d, floor %d)",
					out.Guards, tc.wantGuard, tc.changed, tc.tracked, minDestructiveToGuard)
			}
		})
	}
}

// The empty-side guard has no floor and must not acquire one: a side reading
// empty is not a proportion of anything. One tracked key vanishing from a side
// is the same signal as a thousand, and it matters most on a small rule, whose
// whole scope fits inside the floor the ratio guard uses.
func TestEmptySideGuardHasNoFloor(t *testing.T) {
	h := testHasher(t)
	pair := testPair(config.ModeBidirectional, config.ConflictNewestWins, config.DeletePropagate)

	a := keys(1, "v1")
	st := synced(t, a, h)

	out, err := Reconcile(pair, a, Snapshot{}, st, h, now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := guardNames(out); len(got) != 1 || got[0] != "empty-side" {
		t.Fatalf("guards = %v, want one empty-side even for a single tracked key", out.Guards)
	}
}
