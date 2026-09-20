package state

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// schemaVersion is bumped whenever the meaning of a field changes. A newer
	// file is refused rather than read optimistically: state misread as empty
	// says every secret is new. Version 2 binds the entry key into every hash
	// (see Hasher.Hash), so version 1 hashes agree with it nowhere.
	schemaVersion = 2

	saltLen = 32

	// State is secret-adjacent: it says which keys exist where, and when they
	// changed. Nothing but the owner needs it.
	stateFileMode = 0o600
	stateDirMode  = 0o700
)

// Salt sources. The file records which one wrote it, because switching from
// one to the other changes what every stored hash means.
const (
	saltSourceFile = "file"
	saltSourceEnv  = "env"
)

// FileStore keeps every scope in one JSON file, rewritten atomically on each
// Save.
type FileStore struct {
	path string
	now  func() time.Time
	// salt, when set, was supplied by the caller and is never written to the
	// file. Otherwise the document carries its own.
	salt []byte
	// readOnly takes a shared lock and refuses Save.
	readOnly bool

	mu     sync.Mutex
	doc    *document
	lock   *os.File
	closed bool
}

type document struct {
	Version int `json:"version"`
	// Salt is present only when the store generated it. A salt supplied from
	// the environment stays out of the file: keeping the two apart is the
	// whole reason to supply one.
	Salt       string `json:"salt,omitempty"`
	SaltSource string `json:"saltSource"`
	// SaltFingerprint is a one-way function of the salt, so a salt that
	// changed can be detected without the file holding it.
	SaltFingerprint string `json:"saltFingerprint"`
	// UpdatedAt is the last write of any scope, so an operator can see at a
	// glance whether the mirror has run at all.
	UpdatedAt time.Time             `json:"updatedAt"`
	Scopes    map[string]*RuleState `json:"scopes"`
}

// FileOption customises a FileStore.
type FileOption func(*FileStore)

// WithFileClock replaces the clock, for tests.
func WithFileClock(now func() time.Time) FileOption {
	return func(f *FileStore) { f.now = now }
}

// WithSalt supplies the hashing salt from outside, typically from the variable
// named by state.saltEnv, so it stays out of the file and the file alone cannot
// test a guess against a hash. The cost is that the salt must not be lost:
// changing it voids every recorded hash, which the store refuses to do quietly.
func WithSalt(salt []byte) FileOption {
	return func(f *FileStore) { f.salt = salt }
}

// ReadOnly opens the store for reading: a shared lock rather than an exclusive
// one, and Save refuses. Several readers can hold the file at once so that an
// operator asking what the scheduled pass is about to do is not refused by that
// pass, at the moment they most want an answer. A writer still cannot join.
func ReadOnly() FileOption {
	return func(f *FileStore) { f.readOnly = true }
}

// OpenFile opens, or creates, the state file at path, taking an exclusive
// advisory lock for the life of the store. Two runs sharing a file would each
// write back a document missing the other's work, leaving the loser's secrets
// unsynced; a CronJob overrunning its schedule is how that ordinarily happens.
func OpenFile(path string, opts ...FileOption) (*FileStore, error) {
	store := &FileStore{path: path, now: time.Now}
	for _, opt := range opts {
		opt(store)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		return nil, fmt.Errorf("state: creating %s: %w", dir, err)
	}
	// MkdirAll is a no-op on a directory that already exists, so a pre-created
	// or inherited one keeps whatever mode it had. The file inside stays 0600
	// either way, but anyone who can write the directory can rename a document
	// over it, and the document is what decides which side changed.
	if err := os.Chmod(dir, stateDirMode); err != nil {
		return nil, fmt.Errorf("state: tightening %s to %o: %w", dir, stateDirMode, err)
	}

	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|openLockOnly, stateFileMode)
	if err != nil {
		return nil, fmt.Errorf("state: opening lock file: %w", err)
	}
	if err := lockFile(lock, store.readOnly); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("state: %s: %w", path, err)
	}
	store.lock = lock

	// Under the lock, so no live run's temp file is in reach.
	store.sweepTemp(dir)

	doc, err := store.read()
	if err != nil {
		_ = unlockFile(lock)
		_ = lock.Close()
		return nil, err
	}
	store.doc = doc
	return store, nil
}

// staleTempAge is how long a leftover temp file must have gone untouched before
// it is swept. Comfortably longer than any write takes, and the lock already
// rules out a concurrent run.
const staleTempAge = time.Hour

// sweepTemp removes temp files left by a run killed between creating one and
// renaming it into place. SIGKILL has no error path, and what it leaves is a
// 0600 file holding the salt and every hash. Failures are ignored on purpose: a
// state file that opens is worth more than a temp file that got cleaned up.
func (f *FileStore) sweepTemp(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := f.now().Add(-staleTempAge)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, ".state-") || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// read loads the document, creating a fresh one when the file does not exist
// yet. Anything else, including a truncated or unreadable file, is an error:
// starting from an empty document would tell the reconciler that every secret
// on both sides is brand new.
func (f *FileStore) read() (*document, error) {
	raw, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return f.newDocument()
	}
	if err != nil {
		return nil, fmt.Errorf("state: reading %s: %w", f.path, err)
	}

	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("state: %s is not readable state (refusing to treat it as empty, which would make every secret look new): %w", f.path, err)
	}
	if doc.Version != schemaVersion {
		return nil, fmt.Errorf("state: %s has schema version %d, this build understands %d", f.path, doc.Version, schemaVersion)
	}
	if err := f.adoptSalt(&doc); err != nil {
		return nil, err
	}
	if doc.Scopes == nil {
		doc.Scopes = map[string]*RuleState{}
	}
	return &doc, nil
}

// newDocument starts a fresh state file, either keyed to the salt this run was
// given or to one generated here and kept.
func (f *FileStore) newDocument() (*document, error) {
	doc := &document{Version: schemaVersion, Scopes: map[string]*RuleState{}}

	if len(f.salt) > 0 {
		hasher, err := NewHasher(f.salt)
		if err != nil {
			return nil, err
		}
		doc.SaltSource = saltSourceEnv
		doc.SaltFingerprint = hasher.Fingerprint()
		return doc, nil
	}

	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("state: generating salt: %w", err)
	}
	hasher, err := NewHasher(salt)
	if err != nil {
		return nil, err
	}
	doc.Salt = base64.StdEncoding.EncodeToString(salt)
	doc.SaltSource = saltSourceFile
	doc.SaltFingerprint = hasher.Fingerprint()
	return doc, nil
}

// adoptSalt reconciles the salt this run holds with the one the file was
// written under. A different salt does not fail; it silently makes both sides
// of every pair look changed, which is a mass rewrite under a conflict policy
// and worse under a delete one. Each mismatch stops the run and says which.
func (f *FileStore) adoptSalt(doc *document) error {
	if doc.SaltSource == "" {
		// Only one thing ever wrote a state file without this field, and it
		// always carried its own salt.
		doc.SaltSource = saltSourceFile
	}

	if len(f.salt) > 0 {
		if doc.SaltSource != saltSourceEnv {
			return fmt.Errorf("state: %s carries a salt of its own, so the supplied salt would make every recorded hash meaningless; unset state.saltEnv, or point it at a new state file", f.path)
		}
		hasher, err := NewHasher(f.salt)
		if err != nil {
			return err
		}
		if doc.SaltFingerprint != hasher.Fingerprint() {
			// Both fingerprints are named so an operator holding more than one
			// candidate salt can tell which one this state wants, without the
			// file ever carrying the salt itself.
			return fmt.Errorf("state: the supplied salt (fingerprint %s) is not the one %s was written with (fingerprint %s); it cannot be recovered from the file, so either restore it or point state.file.path at a new file",
				Short(hasher.Fingerprint()), f.path, Short(doc.SaltFingerprint))
		}
		return nil
	}

	if doc.SaltSource == saltSourceEnv {
		return fmt.Errorf("state: %s was written with a salt supplied from the environment (fingerprint %s), which is not stored in the file; set state.saltEnv to the variable holding it",
			f.path, Short(doc.SaltFingerprint))
	}

	salt, err := decodeSalt(doc.Salt)
	if err != nil {
		return fmt.Errorf("state: %s: %w", f.path, err)
	}
	hasher, err := NewHasher(salt)
	if err != nil {
		return err
	}
	if doc.SaltFingerprint == "" {
		// Written before the field existed. Record it on the next save.
		doc.SaltFingerprint = hasher.Fingerprint()
		return nil
	}
	if doc.SaltFingerprint != hasher.Fingerprint() {
		return fmt.Errorf("state: %s has a salt that does not match its own fingerprint, so the file has been edited or truncated", f.path)
	}
	return nil
}

// Load returns a copy of the state for a scope.
//
// The copy matters: the reconciler mutates what it gets while deciding, and a
// rule that aborts on a guard must leave the stored state untouched.
func (f *FileStore) Load(_ context.Context, scope string) (*RuleState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.usable(); err != nil {
		return nil, err
	}

	stored, ok := f.doc.Scopes[scope]
	if !ok {
		state := NewRuleState()
		state.FirstRun = true
		return state, nil
	}

	state := &RuleState{Entries: make(map[string]Entry, len(stored.Entries)), UpdatedAt: stored.UpdatedAt}
	for key, entry := range stored.Entries {
		state.Entries[key] = entry
	}
	return state, nil
}

// Save replaces one scope and rewrites the file before returning.
func (f *FileStore) Save(_ context.Context, scope string, st *RuleState) error {
	if st == nil {
		return fmt.Errorf("state: refusing to save a nil state for scope %q", scope)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.usable(); err != nil {
		return err
	}
	if f.readOnly {
		// The shared lock means a writer may hold the file at the same time, so
		// writing here would lose one of the two documents. A caller reaching
		// this has a bug, not a permissions problem.
		return fmt.Errorf("state: %s was opened read-only", f.path)
	}

	now := f.now()
	saved := &RuleState{Entries: make(map[string]Entry, len(st.Entries)), UpdatedAt: now}
	for key, entry := range st.Entries {
		saved.Entries[key] = entry
	}

	f.doc.Scopes[scope] = saved
	f.doc.UpdatedAt = now
	st.UpdatedAt = now

	if err := f.write(); err != nil {
		// Leave the in-memory document as it is: it now disagrees with disk,
		// and the caller is expected to stop rather than carry on writing
		// secrets whose outcome cannot be recorded.
		return err
	}
	return nil
}

// Hasher returns the hasher keyed with this store's salt.
func (f *FileStore) Hasher(context.Context) (*Hasher, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.usable(); err != nil {
		return nil, err
	}

	if len(f.salt) > 0 {
		return NewHasher(f.salt)
	}
	salt, err := decodeSalt(f.doc.Salt)
	if err != nil {
		return nil, err
	}
	return NewHasher(salt)
}

// SaltFingerprint identifies the salt this state is keyed to, for `state show`
// and for an operator holding more than one candidate salt. It is a one-way
// function of the salt, so it names it without disclosing any part of it.
func (f *FileStore) SaltFingerprint() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.doc == nil {
		return ""
	}
	return f.doc.SaltFingerprint
}

// Scopes lists the scopes the file holds, sorted, for `state show`.
func (f *FileStore) Scopes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.doc == nil {
		return nil
	}

	scopes := make([]string, 0, len(f.doc.Scopes))
	for scope := range f.doc.Scopes {
		scopes = append(scopes, scope)
	}
	slices.Sort(scopes)
	return scopes
}

// Close releases the lock. Saves have already been written, so there is
// nothing to flush here.
func (f *FileStore) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true

	var err error
	if f.lock != nil {
		err = errors.Join(unlockFile(f.lock), f.lock.Close())
		f.lock = nil
	}
	return err
}

func (f *FileStore) usable() error {
	if f.closed {
		return fmt.Errorf("state: %s is closed", f.path)
	}
	if f.doc == nil {
		return fmt.Errorf("state: %s was never opened", f.path)
	}
	return nil
}

// write serialises the document to a temp file in the same directory, fsyncs
// it, and renames it over the target. Same directory because rename is atomic
// only within a filesystem; the fsyncs because a rename that reaches the
// directory before the data leaves an empty state file behind a crash.
func (f *FileStore) write() error {
	raw, err := json.MarshalIndent(f.doc, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encoding %s: %w", f.path, err)
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("state: creating a temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()

	cleanup := func(cause error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return cause
	}

	if err := tmp.Chmod(stateFileMode); err != nil {
		return cleanup(fmt.Errorf("state: securing %s: %w", tmpName, err))
	}
	if _, err := tmp.Write(raw); err != nil {
		return cleanup(fmt.Errorf("state: writing %s: %w", tmpName, err))
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(fmt.Errorf("state: syncing %s: %w", tmpName, err))
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("state: closing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("state: replacing %s: %w", f.path, err)
	}

	// Durably record the rename itself: without this the file can survive a
	// crash while the directory entry pointing at it does not. The errors are
	// reported rather than dropped, or this load-bearing step could be a
	// complete no-op while Save still returned success.
	handle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("state: opening %s to sync the rename: %w", dir, err)
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		if !dirSyncTolerable(err) {
			return fmt.Errorf("state: syncing %s after the rename: %w", dir, err)
		}
		return nil
	}
	return handle.Close()
}

func decodeSalt(encoded string) ([]byte, error) {
	salt, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("salt is not valid base64: %w", err)
	}
	if len(salt) < minSaltLen {
		return nil, fmt.Errorf("salt is %d bytes, want at least %d", len(salt), minSaltLen)
	}
	return salt, nil
}
