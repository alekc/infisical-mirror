package state

import (
	"strings"
	"testing"
	"time"
)

func TestEntryKeyRoundTrip(t *testing.T) {
	tests := []struct {
		relPath, secretKey, want string
	}{
		{"/", "TOKEN", "/|TOKEN"},
		{"", "TOKEN", "/|TOKEN"},
		{"apps/arr", "TOKEN", "/apps/arr|TOKEN"},
		{"/apps/arr/", "TOKEN", "/apps/arr|TOKEN"},
		{"//apps//arr", "TOKEN", "/apps/arr|TOKEN"},
		// A secret key may contain anything, including the separator, which is
		// why only the first one splits.
		{"/apps", "ODD|KEY", "/apps|ODD|KEY"},
	}

	for _, tc := range tests {
		key := EntryKey(tc.relPath, tc.secretKey)
		if key != tc.want {
			t.Errorf("EntryKey(%q, %q) = %q, want %q", tc.relPath, tc.secretKey, key, tc.want)
		}

		gotPath, gotKey, ok := SplitEntryKey(key)
		if !ok {
			t.Errorf("SplitEntryKey(%q) failed", key)
			continue
		}
		if gotKey != tc.secretKey {
			t.Errorf("SplitEntryKey(%q) key = %q, want %q", key, gotKey, tc.secretKey)
		}
		if gotPath != normalizeRel(tc.relPath) {
			t.Errorf("SplitEntryKey(%q) path = %q, want %q", key, gotPath, normalizeRel(tc.relPath))
		}
	}

	if _, _, ok := SplitEntryKey("no-separator"); ok {
		t.Error("a key with no separator should not split")
	}
}

func TestTrackedCountsLiveEntriesOnly(t *testing.T) {
	now := time.Now()
	st := NewRuleState()
	st.Put(EntryKey("/", "A"), "ha", "hb", now)
	st.Put(EntryKey("/apps", "B"), "ha", "hb", now)
	st.Tombstone(EntryKey("/", "GONE"), now)

	// The change-ratio guard divides by this, so a pile of old deletions must
	// not inflate it and make a dangerous run look small.
	if got := st.Tracked(); got != 2 {
		t.Errorf("Tracked = %d, want 2", got)
	}
	if got := len(st.Entries); got != 3 {
		t.Errorf("the tombstone should still be stored, got %d entries", got)
	}
}

// A tombstone keeps no hashes: a key recreated later with its old value must
// not look converged, and a hash of a secret nobody holds should not outlive
// the secret.
func TestTombstoneClearsHashes(t *testing.T) {
	now := time.Now()
	st := NewRuleState()
	key := EntryKey("/", "A")
	st.Put(key, "hash-a", "hash-b", now)
	st.Tombstone(key, now)

	entry, ok := st.Get(key)
	if !ok {
		t.Fatal("the tombstone is missing")
	}
	if !entry.Tombstone {
		t.Error("Tombstone was not set")
	}
	if entry.HashA != "" || entry.HashB != "" {
		t.Errorf("hashes survived the tombstone: %+v", entry)
	}
}

func TestPruneDropsOldTombstonesOnly(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	st := NewRuleState()
	st.Put(EntryKey("/", "LIVE"), "ha", "hb", now.Add(-90*24*time.Hour))
	st.Tombstone(EntryKey("/", "OLD"), now.Add(-90*24*time.Hour))
	st.Tombstone(EntryKey("/", "RECENT"), now.Add(-1*time.Hour))

	if pruned := st.Prune(now.Add(-30 * 24 * time.Hour)); pruned != 1 {
		t.Errorf("Prune removed %d, want 1", pruned)
	}
	// A live entry is never pruned by age: the secret is still there, however
	// long ago it last changed.
	if got := st.Keys(); strings.Join(got, ",") != "/|LIVE,/|RECENT" {
		t.Errorf("remaining keys = %v", got)
	}
}

func TestHasher(t *testing.T) {
	salt := []byte("0123456789abcdef0123456789abcdef")
	h, err := NewHasher(salt)
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}

	const key = "/apps|TOKEN"

	// The two sides are deliberately identical: determinism is the property.
	//nolint:staticcheck // SA4000 is exactly what this asserts.
	if h.Hash(key, "value", "comment") != h.Hash(key, "value", "comment") {
		t.Error("hashing is not deterministic")
	}
	if h.Hash(key, "value", "") == h.Hash(key, "value", "comment") {
		t.Error("the comment must be part of the hash, or a comment-only change never propagates")
	}

	// Length prefixes, not concatenation: each of these pairs concatenates the
	// same way as its partner, and must not hash the same way.
	if h.Hash(key, "ab", "c") == h.Hash(key, "a", "bc") {
		t.Error("value and comment are not unambiguously separated")
	}
	if h.Hash("/a|b", "c", "") == h.Hash("/a|bc", "", "") {
		t.Error("entry key and value are not unambiguously separated")
	}

	other, err := NewHasher([]byte("fedcba9876543210fedcba9876543210"))
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	if h.Hash(key, "value", "") == other.Hash(key, "value", "") {
		t.Error("two salts produced the same hash")
	}

	// The hash must not be reversible to the value by eye, and must not carry
	// it around in a state file.
	if hash := h.Hash(key, "super-secret", ""); strings.Contains(hash, "super-secret") {
		t.Errorf("hash contains the value: %q", hash)
	}
}

// Without the entry key in the MAC, anyone holding the state file can read off
// which secrets share a value: not what they are, but which of them are the
// same, across every folder and every environment in the document. That is a
// credential-reuse map, and it is free to whoever gets the file.
func TestHasherDoesNotRevealThatTwoKeysShareAValue(t *testing.T) {
	h, err := NewHasher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}

	same := h.Hash(EntryKey("/dev", "DB_PASSWORD"), "hunter2", "")
	elsewhere := h.Hash(EntryKey("/prod", "DB_PASSWORD"), "hunter2", "")
	if same == elsewhere {
		t.Error("the same value under two entry keys hashes identically, so the file discloses value equality")
	}

	// The property the reconciler depends on is the other half of this: within
	// one entry key, the same value must still hash the same way, or nothing
	// ever converges.
	if again := h.Hash(EntryKey("/dev", "DB_PASSWORD"), "hunter2", ""); again != same {
		t.Error("the same value under one entry key hashed differently")
	}
}

func TestNewHasherRejectsWeakSalt(t *testing.T) {
	for _, salt := range [][]byte{nil, {}, []byte("short")} {
		if _, err := NewHasher(salt); err == nil {
			t.Errorf("a %d byte salt should be rejected", len(salt))
		}
	}
}

func TestShort(t *testing.T) {
	if got := Short("0123456789abcdef"); got != "01234567" {
		t.Errorf("Short = %q", got)
	}
	if got := Short("abc"); got != "abc" {
		t.Errorf("Short of a short string = %q", got)
	}
}
