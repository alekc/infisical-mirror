// Package reconcile turns two sides of a rule into the list of changes that
// would bring them into step. The decision is pure: two snapshots plus the
// last-synced state in, a plan out. Reading the sides and carrying the changes
// out live elsewhere, so the decision table is testable without a server.
package reconcile

import (
	"fmt"
	"sort"
	"time"

	"github.com/alekc/infisical-mirror/internal/config"
	"github.com/alekc/infisical-mirror/internal/state"
)

// Side names one of a pair's two endpoints.
type Side string

const (
	SideA Side = "a"
	SideB Side = "b"
)

// Other returns the opposite side.
func (s Side) Other() Side {
	if s == SideA {
		return SideB
	}
	return SideA
}

// Op is what one entry in a plan would do.
type Op string

const (
	// OpCreate writes a key that the target side does not have.
	OpCreate Op = "create"
	// OpUpdate overwrites a key the target side has with a different value.
	OpUpdate Op = "update"
	// OpDelete removes a key from the target side.
	OpDelete Op = "delete"
	// OpConflict means both sides changed and nothing will be written. It is
	// reported so a human can resolve it, and repeats every run until they do.
	OpConflict Op = "conflict"
	// OpDeleteObserved means a key was deleted on one side and `delete:
	// ignore` left the surviving copy alone. Also reported every run: the
	// state entry is deliberately kept, and keeping it is what stops the next
	// pass reading the survivor as a new secret and resurrecting it.
	OpDeleteObserved Op = "delete-observed"
)

// Writes reports whether this op changes a side.
func (o Op) Writes() bool {
	return o == OpCreate || o == OpUpdate || o == OpDelete
}

// Destructive reports whether this op overwrites or removes existing data.
// Creating a key destroys nothing, which is why a first run, where everything
// is a create, does not trip the change-ratio guard.
func (o Op) Destructive() bool {
	return o == OpUpdate || o == OpDelete
}

// Secret is one secret as observed on a side, reduced to what reconciling
// needs. Values are already normalised by the client.
type Secret struct {
	Value     string
	Comment   string
	UpdatedAt time.Time
}

// Snapshot is one side's secrets, keyed by state.EntryKey(relPath, key), where
// relPath is relative to that side's root folder. Keying on the relative path
// is what lets two differently rooted folder trees be compared at all.
type Snapshot map[string]Secret

// Action is one entry in a plan. The secret's value is held unexported so that
// nothing outside this package can print it: a plan reaches a terminal and CI
// logs, and an exported value field would reach both through a single %+v.
type Action struct {
	Op      Op
	Side    Side // side to be written; empty for conflicts and observed deletes
	RelPath string
	Key     string
	Reason  string

	value   string
	comment string

	// entryKey, prev and hadPrev let a failed write be undone in the state
	// document. Outcome.Next assumes every action succeeded, so persisting it
	// after a partial failure records two sides as agreeing on a secret that was
	// never written, and the next pass sees no drift and never retries.
	entryKey string
	prev     state.Entry
	hadPrev  bool
}

// String renders an action without its value. fmt prints unexported fields
// through reflection, so %+v on an Action would otherwise leak the secret;
// defining String is what keeps that from happening.
func (a Action) String() string {
	target := string(a.Side)
	if target == "" {
		target = "-"
	}
	return fmt.Sprintf("%s %s %s:%s (%s)", a.Op, target, a.RelPath, a.Key, a.Reason)
}

// Guard is a refusal: a reason this rule should not be applied as planned.
type Guard struct {
	Name   string
	Detail string
}

func (g Guard) String() string { return g.Name + ": " + g.Detail }

// Outcome is the result of reconciling one pair.
type Outcome struct {
	Actions []Action
	Guards  []Guard
	// Converged counts keys already identical on both sides.
	Converged int
	// Untracked counts keys present on one side only that this pair will not
	// write anywhere: a one-way rule never creates on its source side, and
	// never deletes a key its source never had.
	Untracked int
	// OutOfScope counts state entries whose folder the rule no longer selects.
	// They are carried forward untouched rather than read as deletions.
	OutOfScope int
	// Retired counts keys that were tracked and have since gone from both
	// sides. Nothing is written for them, but their state entry becomes a
	// tombstone, and a state change nothing reports is a change nobody
	// approved.
	Retired int
	// InScope is how many live state entries this rule still selects, which is
	// the denominator both guards measure against. It is not the same as the
	// number of keys the state file holds for this pair: a rule narrowed by an
	// exclude leaves entries behind that it no longer looks at.
	InScope int
	// Next is the state as it would be after every action succeeded.
	Next *state.RuleState
}

// Writes counts the actions that would change something.
func (o *Outcome) Writes() int { return o.count(Op.Writes) }

// Destructive counts the actions that would overwrite or delete.
func (o *Outcome) Destructive() int { return o.count(Op.Destructive) }

// Conflicts counts the keys left for a human.
func (o *Outcome) Conflicts() int {
	n := 0
	for _, a := range o.Actions {
		if a.Op == OpConflict {
			n++
		}
	}
	return n
}

func (o *Outcome) count(pred func(Op) bool) int {
	n := 0
	for _, a := range o.Actions {
		if pred(a.Op) {
			n++
		}
	}
	return n
}

// Blocked reports whether a guard fired, meaning this outcome must not be
// applied without an explicit override.
func (o *Outcome) Blocked() bool { return len(o.Guards) > 0 }

// Reconcile decides what would have to change for the two snapshots to agree,
// and what the sync state would look like once it had. It writes nothing: st is
// left untouched, and the updated state comes back as Outcome.Next.
func Reconcile(pair config.Pair, a, b Snapshot, st *state.RuleState, h *state.Hasher, now time.Time) (*Outcome, error) {
	if h == nil {
		return nil, fmt.Errorf("reconcile: a hasher is required")
	}
	if st == nil {
		st = state.NewRuleState()
	}

	r := &run{
		pair:   pair,
		hasher: h,
		now:    now,
		out:    &Outcome{Next: cloneState(st)},
	}

	for _, key := range unionKeys(a, b, st) {
		rel, name, ok := state.SplitEntryKey(key)
		if !ok {
			return nil, fmt.Errorf("reconcile: malformed state key %q", key)
		}
		if !pair.Selects(rel) {
			// Only reachable from state: the snapshots are filtered before
			// they get here. A rule that has been narrowed since the last run
			// leaves its old keys alone rather than treating a folder that is
			// no longer looked at as a folder that was emptied.
			r.out.OutOfScope++
			continue
		}

		as, hasA := a[key]
		bs, hasB := b[key]
		entry, hasEntry := st.Get(key)
		if hasEntry && !entry.Tombstone {
			r.tracked++
		}

		r.decide(item{
			key:     key,
			relPath: rel,
			name:    name,
			a:       optional(as, hasA),
			b:       optional(bs, hasB),
			entry:   entry,
			known:   hasEntry,
		})
	}

	sort.SliceStable(r.out.Actions, func(i, j int) bool {
		x, y := r.out.Actions[i], r.out.Actions[j]
		if x.RelPath != y.RelPath {
			return x.RelPath < y.RelPath
		}
		return x.Key < y.Key
	})

	r.out.InScope = r.tracked
	r.guard(a, b)
	r.out.Next.UpdatedAt = now
	return r.out, nil
}

// item is one key with everything known about it.
type item struct {
	key     string
	relPath string
	name    string
	a       *Secret
	b       *Secret
	entry   state.Entry
	known   bool
}

// present returns the given side's secret, or nil.
func (i item) present(s Side) *Secret {
	if s == SideA {
		return i.a
	}
	return i.b
}

// lastHash returns the hash recorded for one side by the previous run.
func (i item) lastHash(s Side) string {
	if s == SideA {
		return i.entry.HashA
	}
	return i.entry.HashB
}

// tracked reports whether the previous run recorded this key as live on the
// given side. A tombstone does not count: a key that comes back after being
// deleted everywhere is a new secret, and is propagated like one.
func (i item) tracked(s Side) bool {
	return i.known && !i.entry.Tombstone && i.lastHash(s) != ""
}

type run struct {
	pair   config.Pair
	hasher *state.Hasher
	now    time.Time
	out    *Outcome
	// tracked counts the live state entries this rule still selects. It is
	// what the guards measure against, rather than every entry in the scope:
	// a rule narrowed by an exclude keeps its old entries, and counting them
	// would leave both guards permanently measuring folders nobody reads.
	tracked int
}

func (r *run) decide(i item) {
	switch {
	case i.a != nil && i.b != nil:
		r.bothPresent(i)
	case i.a != nil:
		r.oneSide(i, SideA)
	case i.b != nil:
		r.oneSide(i, SideB)
	default:
		// Known to state, gone from both sides. Nothing to write, but the state
		// entry changes, and a change to state that no counter and no line
		// reports is a plan whose stated effect is not its whole effect.
		if !i.entry.Tombstone {
			r.out.Next.Tombstone(i.key, r.now)
			r.out.Retired++
		}
	}
}

// bothPresent handles a key that exists on both sides.
func (r *run) bothPresent(i item) {
	ha, hb := r.hash(i, *i.a), r.hash(i, *i.b)
	if ha == hb {
		r.out.Converged++
		r.out.Next.Put(i.key, ha, hb, r.now)
		return
	}

	// What differs matters to whoever reads the plan: an update on a secret
	// reads as "the value changed" unless it says otherwise, and a comment
	// the two sides disagree about is not the same news at all.
	what := differs(*i.a, *i.b)

	switch r.pair.Mode {
	case config.ModeAToB:
		r.write(i, OpUpdate, SideB, *i.a, what+" differs from the source")
		return
	case config.ModeBToA:
		r.write(i, OpUpdate, SideA, *i.b, what+" differs from the source")
		return
	}

	// Bidirectional: the last-synced hashes are what say which side moved.
	if i.tracked(SideA) && i.tracked(SideB) {
		changedA := i.entry.HashA != ha
		changedB := i.entry.HashB != hb
		switch {
		case changedA && !changedB:
			r.write(i, OpUpdate, SideB, *i.a, what+" changed on a since the last sync")
			return
		case changedB && !changedA:
			r.write(i, OpUpdate, SideA, *i.b, what+" changed on b since the last sync")
			return
		}
	}
	r.resolve(i)
}

// differs names what the two sides disagree about. A comment-only difference
// is real work for the mirror and is mirrored like any other, but it is not
// the change an operator assumes when they read "update" against a secret.
func differs(a, b Secret) string {
	switch {
	case a.Value != b.Value && a.Comment != b.Comment:
		return "value and comment"
	case a.Comment != b.Comment:
		return "comment"
	default:
		return "value"
	}
}

// resolve applies the conflict policy to a key both sides changed, or that
// there is no state for.
func (r *run) resolve(i item) {
	switch r.pair.Conflict {
	case config.ConflictAWins:
		r.write(i, OpUpdate, SideB, *i.a, "conflict, a-wins")
	case config.ConflictBWins:
		r.write(i, OpUpdate, SideA, *i.b, "conflict, b-wins")
	case config.ConflictNewestWins:
		switch {
		case i.a.UpdatedAt.IsZero() || i.b.UpdatedAt.IsZero():
			// No usable update time on one side, because the field was absent
			// or did not parse. A zero time is not an old time: comparing it
			// would silently hand every conflict to the other side, which is
			// the whole estate on an instance whose timestamp format moved.
			r.conflict(i, fmt.Sprintf("newest-wins cannot choose: %s has no usable update time", zeroSides(i)))
		case i.a.UpdatedAt.After(i.b.UpdatedAt):
			r.write(i, OpUpdate, SideB, *i.a, fmt.Sprintf("conflict, newest-wins: a is newer (%s)", stamp(i.a.UpdatedAt)))
		case i.b.UpdatedAt.After(i.a.UpdatedAt):
			r.write(i, OpUpdate, SideA, *i.b, fmt.Sprintf("conflict, newest-wins: b is newer (%s)", stamp(i.b.UpdatedAt)))
		default:
			// Equal, or both missing a usable timestamp. Picking a side on a
			// tie would be a coin toss that overwrites a real secret.
			r.conflict(i, fmt.Sprintf("newest-wins cannot choose: both sides carry the same update time (%s)", stamp(i.a.UpdatedAt)))
		}
	default:
		r.conflict(i, "both sides changed since the last sync")
	}
}

// zeroSides names the side or sides with no usable update time, so the reason
// on a conflict line says where to look.
func zeroSides(i item) string {
	switch {
	case i.a.UpdatedAt.IsZero() && i.b.UpdatedAt.IsZero():
		return "neither side"
	case i.a.UpdatedAt.IsZero():
		return "side a"
	default:
		return "side b"
	}
}

// oneSide handles a key that only one side has.
func (r *run) oneSide(i item, present Side) {
	missing := present.Other()
	secret := *i.present(present)

	// The pair never writes to the side that has it, so the question is only
	// ever what to do about the side that does not.
	if !r.writes(missing) {
		// One-way, and the key is missing on the source. A key the source
		// never had is not a reason to touch anything, and a key the source
		// deleted is handled below by the delete policy, not here.
		if !i.tracked(missing) {
			r.out.Untracked++
			return
		}
		r.deleted(i, missing, present, secret)
		return
	}

	if !i.tracked(missing) {
		// New on the side that has it. This includes a key that carries a
		// tombstone: deleted everywhere once, added again since, and a human
		// adding a secret back expects it to sync like any other.
		r.write(i, OpCreate, missing, secret, fmt.Sprintf("new on %s", present))
		return
	}
	r.deleted(i, missing, present, secret)
}

// deleted handles a key the previous run saw on both sides that has since
// disappeared from one of them.
func (r *run) deleted(i item, gone, survivor Side, secret Secret) {
	// A one-way rule's destination is a copy, so a key deleted there is not a
	// deletion at all: the source still has it, and the mirror restores it.
	if r.pair.Mode != config.ModeBidirectional && r.writes(gone) {
		r.write(i, OpCreate, gone, secret, fmt.Sprintf("deleted on %s, restored from the source", gone))
		return
	}

	// Bidirectional, and the surviving side was edited after the last sync:
	// one side deleted the key while the other changed it. The delete policy
	// and the conflict policy disagree about that, and either answer destroys
	// real work, so it is never resolved automatically.
	if r.pair.Mode == config.ModeBidirectional && r.hash(i, secret) != i.lastHash(survivor) {
		r.conflict(i, fmt.Sprintf("deleted on %s, changed on %s", gone, survivor))
		return
	}

	if r.pair.Delete != config.DeletePropagate {
		// Reported, not acted on, and the state entry is left exactly as it
		// was. Dropping it here is what would make the next pass read the
		// survivor as a brand new secret and put the deleted key back.
		r.out.Actions = append(r.out.Actions, Action{
			Op:      OpDeleteObserved,
			RelPath: i.relPath,
			Key:     i.name,
			Reason:  fmt.Sprintf("deleted on %s, kept on %s (delete: ignore)", gone, survivor),
		})
		return
	}

	r.out.Actions = append(r.out.Actions, Action{
		Op:       OpDelete,
		Side:     survivor,
		RelPath:  i.relPath,
		Key:      i.name,
		Reason:   fmt.Sprintf("deleted on %s (delete: propagate)", gone),
		entryKey: i.key,
		prev:     i.entry,
		hadPrev:  i.known,
	})
	r.out.Next.Tombstone(i.key, r.now)
}

// write records a change and the state it would leave behind. After a
// successful write both sides hold the same value, so both hashes are the
// hash of the value being written.
func (r *run) write(i item, op Op, side Side, src Secret, reason string) {
	h := r.hash(i, src)
	r.out.Actions = append(r.out.Actions, Action{
		Op:       op,
		Side:     side,
		RelPath:  i.relPath,
		Key:      i.name,
		Reason:   reason,
		value:    src.Value,
		comment:  src.Comment,
		entryKey: i.key,
		prev:     i.entry,
		hadPrev:  i.known,
	})
	r.out.Next.Put(i.key, h, h, r.now)
}

// conflict records a key that needs a human. The state entry is left as it
// was: nothing was written, so nothing about the last sync has changed.
func (r *run) conflict(i item, reason string) {
	r.out.Actions = append(r.out.Actions, Action{
		Op:      OpConflict,
		RelPath: i.relPath,
		Key:     i.name,
		Reason:  reason,
	})
}

func (r *run) writes(s Side) bool {
	switch r.pair.Mode {
	case config.ModeAToB:
		return s == SideB
	case config.ModeBToA:
		return s == SideA
	default:
		return true
	}
}

// hash keys the digest on the entry it describes, so two secrets that happen to
// hold the same value do not produce the same hash in the state file.
func (r *run) hash(i item, s Secret) string { return r.hasher.Hash(i.key, s.Value, s.Comment) }

// minDestructiveToGuard is the floor under which the change-ratio guard does
// not fire, whatever the ratio works out to. Rotating one secret in a folder of
// three is 33%, so without a floor the guard blocks the most ordinary operation
// there is and operators turn it off everywhere. See docs/design.md.
const minDestructiveToGuard = 3

// guard evaluates the two refusals that stop a plan from being applied.
func (r *run) guard(a, b Snapshot) {
	tracked := r.tracked

	// An expired token, a revoked permission and a genuinely emptied folder
	// all answer a list call the same way, and one of those three is a
	// mass deletion. Only state can tell them apart, and only by refusing.
	if tracked > 0 {
		for _, side := range []struct {
			name Side
			snap Snapshot
		}{{SideA, a}, {SideB, b}} {
			if len(side.snap) == 0 {
				r.out.Guards = append(r.out.Guards, Guard{
					Name:   "empty-side",
					Detail: fmt.Sprintf("side %s returned no secrets in scope while state tracks %d key(s)", side.name, tracked),
				})
			}
		}
	}

	destructive := r.out.Destructive()
	if destructive < minDestructiveToGuard {
		return
	}

	// Tracked keys are the denominator: how much of what this rule already syncs
	// is about to be overwritten or removed. A first run tracks nothing, so the
	// keys in play stand in; without that, a first run against two populated
	// sides could overwrite everything with no ratio to measure it against.
	denom := tracked
	if denom == 0 {
		denom = len(union(a, b))
	}
	if denom == 0 {
		return
	}

	ratio := float64(destructive) / float64(denom)
	if ratio > r.pair.MaxChangeRatio {
		r.out.Guards = append(r.out.Guards, Guard{
			Name: "max-change-ratio",
			Detail: fmt.Sprintf("%d of %d key(s) would be overwritten or deleted (%.0f%%), over the %.0f%% limit",
				destructive, denom, ratio*100, r.pair.MaxChangeRatio*100),
		})
	}
}

// unionKeys returns every key either side or the state knows about, sorted so
// that a plan is reproducible.
func unionKeys(a, b Snapshot, st *state.RuleState) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var keys []string
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range a {
		add(k)
	}
	for k := range b {
		add(k)
	}
	for _, k := range st.Keys() {
		add(k)
	}
	sort.Strings(keys)
	return keys
}

// union counts the distinct keys the two sides hold between them.
func union(a, b Snapshot) map[string]bool {
	keys := make(map[string]bool, len(a)+len(b))
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	return keys
}

func cloneState(st *state.RuleState) *state.RuleState {
	next := state.NewRuleState()
	for k, v := range st.Entries {
		next.Entries[k] = v
	}
	next.UpdatedAt = st.UpdatedAt
	return next
}

func optional(s Secret, ok bool) *Secret {
	if !ok {
		return nil
	}
	return &s
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "no timestamp"
	}
	return t.UTC().Format(time.RFC3339)
}
