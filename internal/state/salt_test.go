package state

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testSalt = "a-salt-long-enough-to-be-accepted"

func TestSaltFromEnv(t *testing.T) {
	t.Setenv("MIRROR_SALT", "  "+testSalt+"\n")
	t.Setenv("MIRROR_SALT_BLANK", "   ")
	t.Setenv("MIRROR_SALT_SHORT", "too-short")

	// Trimmed, because a salt that differs by an invisible trailing newline
	// picked up from a shell or a mounted file invalidates every hash under it.
	salt, err := SaltFromEnv("MIRROR_SALT")
	if err != nil {
		t.Fatalf("SaltFromEnv: %v", err)
	}
	if string(salt) != testSalt {
		t.Errorf("salt = %q, want it trimmed to %q", salt, testSalt)
	}

	rejects := map[string]string{
		"unset":    "MIRROR_SALT_MISSING",
		"blank":    "MIRROR_SALT_BLANK",
		"too weak": "MIRROR_SALT_SHORT",
		"no name":  "",
	}
	for name, envVar := range rejects {
		t.Run(name, func(t *testing.T) {
			_, err := SaltFromEnv(envVar)
			if err == nil {
				t.Fatalf("want an error for %q", envVar)
			}
			// The message names the variable, never its contents.
			if strings.Contains(err.Error(), testSalt) || strings.Contains(err.Error(), "too-short") {
				t.Errorf("error leaked the salt: %v", err)
			}
		})
	}
}

// A salt supplied from outside stays outside: keeping it out of the file is
// the only reason to supply one.
func TestExternalSaltIsNotWrittenToTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	ctx := context.Background()

	store, err := OpenFile(path, WithSalt([]byte(testSalt)))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer store.Close()

	if err := store.Save(ctx, "rule-a", NewRuleState()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), testSalt) {
		t.Fatal("the state file contains the supplied salt")
	}
	if strings.Contains(string(raw), base64.StdEncoding.EncodeToString([]byte(testSalt))) {
		t.Fatal("the state file contains the supplied salt, base64 encoded")
	}

	var doc struct {
		Salt            string `json:"salt"`
		SaltSource      string `json:"saltSource"`
		SaltFingerprint string `json:"saltFingerprint"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Salt != "" {
		t.Errorf("salt field = %q, want it absent", doc.Salt)
	}
	if doc.SaltSource != saltSourceEnv {
		t.Errorf("saltSource = %q, want %q", doc.SaltSource, saltSourceEnv)
	}
	if doc.SaltFingerprint == "" {
		t.Error("no fingerprint was recorded, so a changed salt could not be detected")
	}
}

func TestExternalSaltRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	ctx := context.Background()
	now := time.Now()

	store, err := OpenFile(path, WithSalt([]byte(testSalt)))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	hasher, err := store.Hasher(ctx)
	if err != nil {
		t.Fatalf("Hasher: %v", err)
	}
	st := NewRuleState()
	st.Put(EntryKey("/", "A"), hasher.Hash(EntryKey("/", "A"), "v", ""), hasher.Hash(EntryKey("/", "A"), "v", ""), now)
	if err := store.Save(ctx, "rule-a", st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenFile(path, WithSalt([]byte(testSalt)))
	if err != nil {
		t.Fatalf("OpenFile with the same salt: %v", err)
	}
	defer reopened.Close()

	second, err := reopened.Hasher(ctx)
	if err != nil {
		t.Fatalf("Hasher: %v", err)
	}
	// The whole point: a hash recorded last run still means the same thing.
	if second.Hash(EntryKey("/", "A"), "v", "") != hasher.Hash(EntryKey("/", "A"), "v", "") {
		t.Fatal("the same salt produced a different hash after a reopen")
	}
	loaded, err := reopened.Load(ctx, "rule-a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if entry, _ := loaded.Get(EntryKey("/", "A")); entry.HashA != second.Hash(EntryKey("/", "A"), "v", "") {
		t.Errorf("stored hash no longer matches the recomputed one: %+v", entry)
	}
}

// Each of these would silently make both sides of every pair look changed,
// which is a mass rewrite under a conflict policy and worse under a delete
// policy. All four have to stop the run and say which way it went.
func TestSaltMismatchesRefuseToRun(t *testing.T) {
	tests := map[string]struct {
		first, second []byte
		wantContains  string
	}{
		"a different external salt": {
			first: []byte(testSalt), second: []byte("a-completely-different-salt-value"),
			wantContains: "not the one",
		},
		"the external salt withdrawn": {
			first: []byte(testSalt), second: nil,
			wantContains: "not stored in the file",
		},
		"an external salt introduced": {
			first: nil, second: []byte(testSalt),
			wantContains: "carries a salt of its own",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			ctx := context.Background()

			var opts []FileOption
			if tc.first != nil {
				opts = append(opts, WithSalt(tc.first))
			}
			store, err := OpenFile(path, opts...)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			if err := store.Save(ctx, "rule-a", NewRuleState()); err != nil {
				t.Fatalf("Save: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			opts = nil
			if tc.second != nil {
				opts = append(opts, WithSalt(tc.second))
			}
			reopened, err := OpenFile(path, opts...)
			if err == nil {
				_ = reopened.Close()
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.wantContains) {
				t.Errorf("error should contain %q, got: %v", tc.wantContains, err)
			}
			// The salt itself must never appear, in whole or in part: that is
			// the entire reason for keeping it out of the file.
			for _, salt := range [][]byte{tc.first, tc.second} {
				if len(salt) == 0 {
					continue
				}
				if strings.Contains(err.Error(), string(salt[:minSaltLen/2])) {
					t.Errorf("the error leaked part of a salt: %v", err)
				}
			}
			// It must say how to proceed, since the salt is unrecoverable.
			if !strings.Contains(err.Error(), "state.saltEnv") && !strings.Contains(err.Error(), "state.file.path") {
				t.Errorf("error should name the setting to change, got: %v", err)
			}
		})
	}
}

// A stored salt swapped by hand, leaving the fingerprint behind, is an edited
// file rather than a usable one.
func TestTamperedStoredSaltIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	ctx := context.Background()

	store, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if err := store.Save(ctx, "rule-a", NewRuleState()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	doc["salt"] = base64.StdEncoding.EncodeToString([]byte("a-different-salt-entirely-here"))
	edited, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	reopened, err := OpenFile(path)
	if err == nil {
		_ = reopened.Close()
		t.Fatal("want an error for a salt that does not match its fingerprint")
	}
	if !strings.Contains(err.Error(), "fingerprint") {
		t.Errorf("error = %v", err)
	}
}

// A file written before the fingerprint field existed still opens, and gets
// the field on its next write.
func TestStoredSaltWithoutFingerprintIsAdopted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	ctx := context.Background()
	salt := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	legacy := `{"version":2,"salt":"` + salt + `","updatedAt":"2026-09-18T12:00:00Z","scopes":{}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	store, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile on a file with no saltSource: %v", err)
	}
	defer store.Close()

	if err := store.Save(ctx, "rule-a", NewRuleState()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var doc struct {
		SaltSource      string `json:"saltSource"`
		SaltFingerprint string `json:"saltFingerprint"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.SaltSource != saltSourceFile || doc.SaltFingerprint == "" {
		t.Errorf("after a save: source = %q, fingerprint = %q", doc.SaltSource, doc.SaltFingerprint)
	}
}

func TestFingerprintIdentifiesWithoutDisclosing(t *testing.T) {
	first, err := NewHasher([]byte(testSalt))
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	second, err := NewHasher([]byte("a-completely-different-salt-value"))
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}

	// Deliberately the same expression twice: the fingerprint must be stable
	// across calls, not merely computable.
	//nolint:staticcheck // SA4000 is exactly what this asserts.
	if first.Fingerprint() != first.Fingerprint() {
		t.Error("the fingerprint is not stable")
	}
	if first.Fingerprint() == second.Fingerprint() {
		t.Error("two salts share a fingerprint")
	}
	if strings.Contains(first.Fingerprint(), testSalt) {
		t.Error("the fingerprint contains the salt")
	}
	// It must also not be the hash of anything a caller might store, or the
	// file would disclose one known plaintext's hash under the salt.
	if first.Fingerprint() == first.Hash("", "", "") {
		t.Error("the fingerprint collides with a hashable value")
	}
}

// Refusing a short salt at the store is the backstop for a caller that did not
// come through SaltFromEnv.
func TestWithSaltRejectsWeakSalt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenFile(path, WithSalt([]byte("short")))
	if err == nil {
		_ = store.Close()
		t.Fatal("want an error for a short salt")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("a rejected open should not have created a state file")
	}
}

// An operator with two candidate salts has to be able to tell which one the
// state wants. The fingerprint is how, and it is a one-way function of the
// salt rather than a piece of it.
func TestSaltMismatchErrorIdentifiesBothSalts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	ctx := context.Background()
	wrong := []byte("a-completely-different-salt-value")

	store, err := OpenFile(path, WithSalt([]byte(testSalt)))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if err := store.Save(ctx, "rule-a", NewRuleState()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	want := store.SaltFingerprint()
	if want == "" {
		t.Fatal("no fingerprint was recorded")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	wrongHasher, err := NewHasher(wrong)
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}

	reopened, openErr := OpenFile(path, WithSalt(wrong))
	if openErr == nil {
		_ = reopened.Close()
		t.Fatal("want an error")
	}
	for _, fingerprint := range []string{Short(want), Short(wrongHasher.Fingerprint())} {
		if !strings.Contains(openErr.Error(), fingerprint) {
			t.Errorf("error should name fingerprint %s, got: %v", fingerprint, openErr)
		}
	}
}
