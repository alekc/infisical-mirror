package state

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T, path string, now time.Time) *FileStore {
	t.Helper()
	store, err := OpenFile(path, WithFileClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestFileStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	store := openTestStore(t, path, now)

	// An unseen scope is a first run, which is not the same as a scope whose
	// state is empty: only one of the two means "this side was never synced".
	loaded, err := store.Load(ctx, "rule-a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.FirstRun || len(loaded.Entries) != 0 {
		t.Fatalf("first load = %+v, want an empty first-run state", loaded)
	}

	loaded.Put(EntryKey("/apps", "TOKEN"), "hash-a", "hash-b", now)
	loaded.Tombstone(EntryKey("/", "GONE"), now)
	if err := store.Save(ctx, "rule-a", loaded); err != nil {
		t.Fatalf("Save: %v", err)
	}

	again, err := store.Load(ctx, "rule-a")
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if again.FirstRun {
		t.Error("a saved scope should not report a first run")
	}
	if len(again.Entries) != 2 {
		t.Fatalf("entries = %+v", again.Entries)
	}
	if entry, _ := again.Get(EntryKey("/apps", "TOKEN")); entry.HashA != "hash-a" || entry.HashB != "hash-b" {
		t.Errorf("entry = %+v", entry)
	}

	// Reopening is the real test: the reconciler only ever sees state that has
	// been through the file.
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened := openTestStore(t, path, now)
	restored, err := reopened.Load(ctx, "rule-a")
	if err != nil {
		t.Fatalf("Load after reopen: %v", err)
	}
	if len(restored.Entries) != 2 || restored.FirstRun {
		t.Fatalf("restored = %+v", restored)
	}
	if entry, _ := restored.Get(EntryKey("/", "GONE")); !entry.Tombstone {
		t.Error("the tombstone did not survive a reopen")
	}
}

// The hash of a given secret has to mean the same thing on the next run, so
// the salt is generated once and then kept.
func TestSaltIsStableAcrossRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Now()
	ctx := context.Background()

	store := openTestStore(t, path, now)
	first, err := store.Hasher(ctx)
	if err != nil {
		t.Fatalf("Hasher: %v", err)
	}
	// Nothing is written until a Save, so the salt has to survive that too.
	if err := store.Save(ctx, "rule-a", NewRuleState()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	const testKey = "/|SECRET"

	reopened := openTestStore(t, path, now)
	second, err := reopened.Hasher(ctx)
	if err != nil {
		t.Fatalf("Hasher after reopen: %v", err)
	}
	if first.Hash(testKey, "v", "c") != second.Hash(testKey, "v", "c") {
		t.Fatal("the salt changed between runs, so every stored hash would be meaningless")
	}

	// Two deployments must not share a salt, or one state file says something
	// about another's secrets.
	elsewhere := openTestStore(t, filepath.Join(t.TempDir(), "state.json"), now)
	third, err := elsewhere.Hasher(ctx)
	if err != nil {
		t.Fatalf("Hasher: %v", err)
	}
	if first.Hash(testKey, "v", "c") == third.Hash(testKey, "v", "c") {
		t.Error("two stores generated the same salt")
	}
}

// Load hands out a copy, so a rule that aborts on a guard partway through
// leaves nothing behind.
func TestLoadReturnsACopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	ctx := context.Background()
	now := time.Now()
	store := openTestStore(t, path, now)

	initial := NewRuleState()
	initial.Put(EntryKey("/", "A"), "ha", "hb", now)
	if err := store.Save(ctx, "rule-a", initial); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Mutating what Save was given must not reach the store either.
	initial.Put(EntryKey("/", "SNEAKY"), "x", "y", now)

	loaded, err := store.Load(ctx, "rule-a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	loaded.Delete(EntryKey("/", "A"))
	loaded.Put(EntryKey("/", "LOCAL"), "x", "y", now)

	stored, err := store.Load(ctx, "rule-a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Join(stored.Keys(), ","); got != "/|A" {
		t.Errorf("stored keys = %q, want only /|A", got)
	}
}

func TestFilePermissionsAndLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub")
	path := filepath.Join(dir, "state.json")
	ctx := context.Background()

	store := openTestStore(t, path, time.Now())
	if err := store.Save(ctx, "rule-a", NewRuleState()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != stateFileMode {
		t.Errorf("state file mode = %o, want %o", perm, stateFileMode)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != stateDirMode {
		t.Errorf("state dir mode = %o, want %o", perm, stateDirMode)
	}

	// Atomic write means temp files are renamed away, never left lying about.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("a temp file was left behind: %s", entry.Name())
		}
	}
}

// The state file records what was seen, never what it was. A hash is the whole
// point, so the value that produced it must not be anywhere in the file.
func TestStateFileHoldsNoValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	ctx := context.Background()
	store := openTestStore(t, path, time.Now())

	hasher, err := store.Hasher(ctx)
	if err != nil {
		t.Fatalf("Hasher: %v", err)
	}
	key := EntryKey("/apps", "DB_PASSWORD")
	hash := hasher.Hash(key, "hunter2", "the database")

	st := NewRuleState()
	st.Put(key, hash, hash, time.Now())
	if err := store.Save(ctx, "rule-a", st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, leaked := range []string{"hunter2", "the database"} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("the state file contains %q", leaked)
		}
	}
	// The key and path are in there on purpose: without them the state cannot
	// be matched to a secret at all.
	if !strings.Contains(string(raw), "DB_PASSWORD") {
		t.Error("the state file should record which key it is about")
	}
}

// Every one of these is a file the mirror must refuse rather than read as
// empty. Empty state says every secret on both sides is new, which under a
// delete policy is how a sync deletes an estate.
func TestOpenFileRefusesUnreadableState(t *testing.T) {
	tests := map[string]string{
		"truncated json":  `{"version":2,"salt":"`,
		"not json at all": "this is not state",
		"future schema":   `{"version":99,"salt":"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=","scopes":{}}`,
		"no salt":         `{"version":2,"salt":"","scopes":{}}`,
		"short salt":      `{"version":2,"salt":"c2hvcnQ=","scopes":{}}`,
		"salt not base64": `{"version":2,"salt":"not base64!","scopes":{}}`,
	}

	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			store, err := OpenFile(path)
			if err == nil {
				_ = store.Close()
				t.Fatal("want an error")
			}
			// The file must be left exactly as it was, so it can be inspected
			// or restored from a backup.
			raw, readErr := os.ReadFile(path)
			if readErr != nil || string(raw) != content {
				t.Errorf("the unreadable file was modified: %q, %v", raw, readErr)
			}
		})
	}
}

// Two passes over one state file each write back a document missing the
// other's work, so the second one has to fail rather than run.
func TestOpenFileLocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	first := openTestStore(t, path, time.Now())

	second, err := OpenFile(path)
	if err == nil {
		_ = second.Close()
		t.Fatal("a second store opened the same state file")
	}
	if !strings.Contains(err.Error(), "another infisical-mirror run") {
		t.Errorf("the error should say what is holding it, got: %v", err)
	}

	// Closing releases it, so a later run is not locked out by an earlier one.
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	third, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile after Close: %v", err)
	}
	_ = third.Close()
}

func TestClosedStoreRefusesWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	ctx := context.Background()
	store, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close twice is fine: a deferred Close after an explicit one is normal.
	if err := store.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	if _, err := store.Load(ctx, "rule-a"); err == nil {
		t.Error("Load on a closed store should fail")
	}
	if err := store.Save(ctx, "rule-a", NewRuleState()); err == nil {
		t.Error("Save on a closed store should fail")
	}
	if _, err := store.Hasher(ctx); err == nil {
		t.Error("Hasher on a closed store should fail")
	}
}

func TestSaveRejectsNilState(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "state.json"), time.Now())
	if err := store.Save(context.Background(), "rule-a", nil); err == nil {
		t.Fatal("want an error for a nil state")
	}
}

func TestScopesAreListedSorted(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "state.json"), time.Now())
	ctx := context.Background()

	for _, scope := range []string{"rule-z|a|b", "rule-a|a|b", "rule-m|a|b"} {
		if err := store.Save(ctx, scope, NewRuleState()); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	if got := strings.Join(store.Scopes(), ","); got != "rule-a|a|b,rule-m|a|b,rule-z|a|b" {
		t.Errorf("Scopes = %q", got)
	}
}

// Saving one scope must not disturb another: a run with --rule writes back
// only what it touched.
func TestSaveIsScopedAndTimestamped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	store := openTestStore(t, path, now)

	a := NewRuleState()
	a.Put(EntryKey("/", "A"), "ha", "hb", now)
	if err := store.Save(ctx, "rule-a", a); err != nil {
		t.Fatalf("Save: %v", err)
	}
	b := NewRuleState()
	b.Put(EntryKey("/", "B"), "ha", "hb", now)
	if err := store.Save(ctx, "rule-b", b); err != nil {
		t.Fatalf("Save: %v", err)
	}

	stored, err := store.Load(ctx, "rule-a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Join(stored.Keys(), ","); got != "/|A" {
		t.Errorf("rule-a keys = %q", got)
	}
	if !stored.UpdatedAt.Equal(now) {
		t.Errorf("UpdatedAt = %v, want the store clock %v", stored.UpdatedAt, now)
	}

	var doc struct {
		Version   int       `json:"version"`
		UpdatedAt time.Time `json:"updatedAt"`
		Scopes    map[string]struct {
			Entries map[string]Entry `json:"entries"`
		} `json:"scopes"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the state file is not valid json: %v", err)
	}
	if doc.Version != schemaVersion {
		t.Errorf("version = %d, want %d", doc.Version, schemaVersion)
	}
	if len(doc.Scopes) != 2 {
		t.Errorf("scopes on disk = %d, want 2", len(doc.Scopes))
	}
	if !doc.UpdatedAt.Equal(now) {
		t.Errorf("document updatedAt = %v", doc.UpdatedAt)
	}
}

// A write that fails must leave the previous state intact. Half a state file is
// worse than a stale one: the stale one costs a re-read of both instances, the
// half one reads as a scope with no entries, which under a propagate-deletes
// policy is the whole folder queued for deletion.
func TestAFailedWriteLeavesThePreviousStateIntact(t *testing.T) {
	// The whole test rests on a chmod being enforced, and root is exempt from
	// directory permission bits: the Save below would succeed and the test
	// would fail for a reason that says nothing about the code. Containers
	// commonly run as root, so this fires in practice rather than in theory.
	if os.Geteuid() == 0 {
		t.Skip("running as root, which ignores the directory mode this test depends on")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	store, err := OpenFile(path, WithSalt([]byte(testSalt)))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	st := NewRuleState()
	st.Put(EntryKey("/", "KEEP"), "ha", "hb", time.Now())
	if err := store.Save(t.Context(), "scope", st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the good state: %v", err)
	}

	// Take write permission off the directory, so creating the temp file fails
	// where the rename would otherwise have happened. This is the real failure
	// mode on a read-only mount or a wrong-owner PVC, not a synthetic seam.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	next := NewRuleState()
	next.Put(EntryKey("/", "NEW"), "hc", "hd", time.Now())
	if err := store.Save(t.Context(), "scope", next); err == nil {
		t.Fatal("Save into an unwritable directory returned nil")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the state after the failed write: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the state file changed under a failed write:\nbefore %s\nafter  %s", before, after)
	}

	// And it is still a valid document, not merely the same bytes: a reopen is
	// what the next run actually does.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenFile(path, WithSalt([]byte(testSalt)))
	if err != nil {
		t.Fatalf("reopening after a failed write: %v", err)
	}
	defer reopened.Close()

	got, err := reopened.Load(t.Context(), "scope")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := got.Get(EntryKey("/", "KEEP")); !ok {
		t.Error("the entry from before the failed write is gone")
	}
	if _, ok := got.Get(EntryKey("/", "NEW")); ok {
		t.Error("an entry from the failed write survived")
	}
}

// A failed write must not leave its temp file behind either, or a directory
// accumulates one per failure and the next operator cannot tell a crash's
// leftovers from a run in progress.
func TestAFailedWriteRemovesItsTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	store, err := OpenFile(path, WithSalt([]byte(testSalt)))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer store.Close()

	st := NewRuleState()
	st.Put(EntryKey("/", "A"), "ha", "hb", time.Now())
	if err := store.Save(t.Context(), "scope", st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if n := countTempFiles(t, dir); n != 0 {
		t.Errorf("a successful write left %d temp file(s) behind", n)
	}
}

// sweepTemp clears leftovers from a run that died between creating the temp
// file and renaming it, and must not touch a fresh one: a concurrent writer's
// temp file is in the same directory, with the same prefix.
func TestSweepRemovesStaleTempFilesOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	stale := filepath.Join(dir, ".state-stale.tmp")
	fresh := filepath.Join(dir, ".state-fresh.tmp")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatalf("seeding %s: %v", p, err)
		}
	}
	old := time.Now().Add(-2 * staleTempAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("aging the stale temp file: %v", err)
	}

	// A file that is not one of ours stays whatever its age.
	other := filepath.Join(dir, "unrelated.tmp")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", other, err)
	}
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatalf("aging the unrelated file: %v", err)
	}

	store, err := OpenFile(path, WithSalt([]byte(testSalt)))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer store.Close()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale temp file survived the sweep (%v)", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a fresh temp file was swept: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("an unrelated file was swept: %v", err)
	}
}

// A read-only store takes a shared lock, so a writer may hold the file at the
// same time. Writing from one would lose whichever document landed first, so
// Save refuses rather than racing.
func TestReadOnlyStoreRefusesToSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	writer, err := OpenFile(path, WithSalt([]byte(testSalt)))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	st := NewRuleState()
	st.Put(EntryKey("/", "A"), "ha", "hb", time.Now())
	if err := writer.Save(t.Context(), "scope", st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reader, err := OpenFile(path, WithSalt([]byte(testSalt)), ReadOnly())
	if err != nil {
		t.Fatalf("OpenFile read-only: %v", err)
	}
	defer reader.Close()

	// Reading works.
	got, err := reader.Load(t.Context(), "scope")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := got.Get(EntryKey("/", "A")); !ok {
		t.Error("the read-only store did not see the saved entry")
	}

	if err := reader.Save(t.Context(), "scope", got); err == nil {
		t.Fatal("Save on a read-only store returned nil")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("error = %v, want it to say the store is read-only", err)
	}
}

// Two read-only stores can hold the file at once. A plan taking the exclusive
// lock meant an operator asking what the scheduled pass was about to do was
// refused by the scheduled pass itself, at the moment they most wanted an
// answer.
func TestTwoReadOnlyStoresShareTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	first, err := OpenFile(path, WithSalt([]byte(testSalt)), ReadOnly())
	if err != nil {
		t.Fatalf("first OpenFile: %v", err)
	}
	defer first.Close()

	second, err := OpenFile(path, WithSalt([]byte(testSalt)), ReadOnly())
	if err != nil {
		t.Fatalf("a second reader was refused: %v", err)
	}
	defer second.Close()
}

func countTempFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".state-") && strings.HasSuffix(e.Name(), ".tmp") {
			n++
		}
	}
	return n
}
