// Package state records what the mirror last saw on each side of a rule, which
// is what tells "created over there" apart from "deleted over here". Entries
// hold keyed hashes and never values, so the file says whether two sides agree
// without saying what they agree on. See docs/design.md.
package state

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// minSaltLen is the shortest salt accepted. A short salt is worse than an
// obviously absent one, because it looks like protection.
const minSaltLen = 16

// Store persists one RuleState per scope. The scope is an opaque string and the
// state is handed over whole rather than field by field so another backend can
// slot in behind this interface; file is the only one today.
type Store interface {
	// Load returns the state for a scope. A scope that has never been synced
	// returns an empty state with FirstRun set, not an error.
	Load(ctx context.Context, scope string) (*RuleState, error)
	// Save replaces the state for a scope and persists it before returning, so
	// a run interrupted halfway keeps the state of the rules it finished.
	Save(ctx context.Context, scope string, st *RuleState) error
	// Hasher returns the hasher keyed with this store's stable salt.
	Hasher(ctx context.Context) (*Hasher, error)
	Close() error
}

// Entry is what the mirror last saw for one secret, on both sides.
type Entry struct {
	// HashA and HashB are keyed hashes of the value and comment last synced on
	// each side. They are empty on a tombstone.
	HashA string `json:"hashA,omitempty"`
	HashB string `json:"hashB,omitempty"`
	// SyncedAt is when this entry was last written, which for a tombstone is
	// when the deletion was recorded. Pruning reads it.
	SyncedAt time.Time `json:"syncedAt"`
	// Tombstone marks a key deleted on both sides. It is kept rather than
	// dropped so a later pass does not read the absence as "new over there"
	// and resurrect the secret.
	Tombstone bool `json:"tombstone,omitempty"`
}

// RuleState is the mirror's memory of one reconcile scope.
type RuleState struct {
	Entries   map[string]Entry `json:"entries"`
	UpdatedAt time.Time        `json:"updatedAt"`

	// FirstRun says this scope had no state at all, as opposed to state with
	// no entries. Not persisted: it is a property of the load, and the two
	// differ in what they mean for an empty side.
	FirstRun bool `json:"-"`
}

// NewRuleState returns an empty state.
func NewRuleState() *RuleState {
	return &RuleState{Entries: map[string]Entry{}}
}

// Get returns the entry for a key.
func (s *RuleState) Get(key string) (Entry, bool) {
	entry, ok := s.Entries[key]
	return entry, ok
}

// Put records both sides of a converged key.
func (s *RuleState) Put(key string, hashA, hashB string, at time.Time) {
	if s.Entries == nil {
		s.Entries = map[string]Entry{}
	}
	s.Entries[key] = Entry{HashA: hashA, HashB: hashB, SyncedAt: at}
}

// Tombstone records that a key is gone from both sides. The hashes are cleared
// so a resurrected key carrying the old value does not look converged, and so
// no hash of a retired secret outlives the secret itself.
func (s *RuleState) Tombstone(key string, at time.Time) {
	if s.Entries == nil {
		s.Entries = map[string]Entry{}
	}
	s.Entries[key] = Entry{SyncedAt: at, Tombstone: true}
}

// Delete forgets a key entirely, so the next pass treats it as never seen.
func (s *RuleState) Delete(key string) {
	delete(s.Entries, key)
}

// Tracked counts the keys the mirror believes exist on both sides. Tombstones
// are excluded, which makes this the denominator for the change-ratio guard:
// a run that would touch more than its share of live keys is the dangerous
// one, and counting deletions from last month into that share hides it.
func (s *RuleState) Tracked() int {
	live := 0
	for _, entry := range s.Entries {
		if !entry.Tombstone {
			live++
		}
	}
	return live
}

// Keys returns every key, sorted, so output and tests are stable.
func (s *RuleState) Keys() []string {
	keys := make([]string, 0, len(s.Entries))
	for key := range s.Entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Prune drops tombstones last written before the cutoff and returns how many
// went. A tombstone only has to outlive the chance of the other side still
// holding the key; keeping them forever grows the file without bound.
func (s *RuleState) Prune(before time.Time) int {
	pruned := 0
	for key, entry := range s.Entries {
		if entry.Tombstone && entry.SyncedAt.Before(before) {
			delete(s.Entries, key)
			pruned++
		}
	}
	return pruned
}

// EntryKey builds the state key for one secret: the folder path relative to the
// rule's root, then the secret key. The separator is safe because Infisical
// restricts folder names to letters, digits, dashes and underscores. A secret
// key can hold anything, so the split below takes only the first separator.
func EntryKey(relPath, secretKey string) string {
	return normalizeRel(relPath) + "|" + secretKey
}

// SplitEntryKey reverses EntryKey.
func SplitEntryKey(key string) (relPath, secretKey string, ok bool) {
	relPath, secretKey, ok = strings.Cut(key, "|")
	if !ok || relPath == "" {
		return "", "", false
	}
	return relPath, secretKey, true
}

// normalizeRel canonicalises a rule-relative folder path so the same folder
// always produces the same key: leading slash, no trailing slash, "/" at the
// root.
func normalizeRel(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// SaltFromEnv reads a salt from the named environment variable. Surrounding
// whitespace is trimmed: a salt differing only by a trailing newline off a
// shell or a mounted file would void every hash recorded under it, invisibly.
func SaltFromEnv(name string) ([]byte, error) {
	if name == "" {
		return nil, errors.New("state: no salt environment variable named")
	}

	salt := strings.TrimSpace(os.Getenv(name))
	if salt == "" {
		return nil, fmt.Errorf("state: environment variable %s is unset or empty", name)
	}
	if len(salt) < minSaltLen {
		return nil, fmt.Errorf("state: the salt in %s is %d bytes, want at least %d", name, len(salt), minSaltLen)
	}
	return []byte(salt), nil
}

// Hasher turns a secret into the keyed hash the state records.
type Hasher struct {
	salt []byte
}

// saltFingerprintLabel is hashed under the salt to produce a value a store can
// record and compare. It is one-way, so the fingerprint says whether the salt
// changed without the file carrying the salt itself.
const saltFingerprintLabel = "infisical-mirror/salt-fingerprint/v1"

// Fingerprint identifies the salt without disclosing it.
func (h *Hasher) Fingerprint() string {
	mac := hmac.New(sha256.New, h.salt)
	mac.Write([]byte(saltFingerprintLabel))
	return hex.EncodeToString(mac.Sum(nil))
}

// NewHasher builds a hasher from a store's salt.
func NewHasher(salt []byte) (*Hasher, error) {
	if len(salt) < minSaltLen {
		return nil, fmt.Errorf("state: salt is %d bytes, want at least %d", len(salt), minSaltLen)
	}
	return &Hasher{salt: salt}, nil
}

// Hash returns the keyed hash of one entry's value and comment, bound to the
// entry key so equal values under different keys do not hash alike. Without
// that binding the file would leak the estate's credential-reuse map. Parts are
// length-prefixed so no triple collides with another. See docs/design.md.
func (h *Hasher) Hash(entryKey, value, comment string) string {
	mac := hmac.New(sha256.New, h.salt)
	fmt.Fprintf(mac, "%d:%s%d:%s%d:%s", len(entryKey), entryKey, len(value), value, len(comment), comment)
	return hex.EncodeToString(mac.Sum(nil))
}

// Short returns the first bytes of a hash, for a log line that has to say
// which version it saw without saying what it was.
func Short(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[:8]
}
