package infisical

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// canary is a string that appears nowhere else, so a match in printed output is
// unambiguously the value having escaped rather than a coincidence.
const canary = "canary-9f3a1c-value"

// printVerbs is every formatting route a value can reach a log by. %#v is the
// one that catches people out: it ignores String and reaches unexported fields
// by reflection, so a type needs GoString as well to be covered.
var printVerbs = []string{"%v", "%+v", "%#v", "%s", "%q"}

// TestNothingPrintsASecretValue is a regression test for the whole surface, not
// for one type: every struct holding a secret value, through every verb,
// alone and nested inside the container that carries it in real code. One
// missing String method in that graph leaks. See docs/design.md.
func TestNothingPrintsASecretValue(t *testing.T) {
	secret := Secret{ID: "s1", Key: "TOKEN", Value: canary, Comment: canary, Path: "/apps"}
	write := SecretWrite{Key: "TOKEN", Value: canary, Comment: canary}

	tok, err := NewStaticToken(canary)
	if err != nil {
		t.Fatalf("NewStaticToken: %v", err)
	}
	ua, err := NewUniversalAuth(canary, canary)
	if err != nil {
		t.Fatalf("NewUniversalAuth: %v", err)
	}
	client, err := New("https://example.invalid", tok)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	subjects := map[string]any{
		"Secret":                 secret,
		"pointer to Secret":      &secret,
		"slice of Secret":        []Secret{secret},
		"map of Secret":          map[string]Secret{"k": secret},
		"SecretWrite":            write,
		"pointer to SecretWrite": &write,
		"slice of SecretWrite":   []SecretWrite{write},
		"WriteRequest":           WriteRequest{ProjectID: "p", Path: "/", Secrets: []SecretWrite{write}},
		"StaticToken":            tok,
		"UniversalAuth":          ua,
		"Client":                 client,
	}

	for name, subject := range subjects {
		t.Run(name, func(t *testing.T) {
			for _, verb := range printVerbs {
				got := fmt.Sprintf(verb, subject)
				if strings.Contains(got, canary) {
					t.Errorf("%s printed the value: %s", verb, got)
				}
			}
		})
	}
}

// Serialising is the other way out. The state file and any future export go
// through encoding/json, and a json:"-" tag that gets dropped in a refactor
// would put every mirrored value on disk in plaintext.
func TestNothingMarshalsASecretValue(t *testing.T) {
	subjects := map[string]any{
		"Secret":       Secret{Key: "TOKEN", Value: canary, Comment: canary},
		"SecretWrite":  SecretWrite{Key: "TOKEN", Value: canary, Comment: canary},
		"WriteRequest": WriteRequest{Secrets: []SecretWrite{{Key: "TOKEN", Value: canary}}},
	}

	for name, subject := range subjects {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(subject)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if strings.Contains(string(raw), canary) {
				t.Errorf("marshalled form carries the value: %s", raw)
			}
		})
	}
}

// The API error carries a server message, and on the endpoints that take or
// return secret material the server message can be the request coming back.
func TestErrorsDoNotCarrySecretBodies(t *testing.T) {
	err := &APIError{
		StatusCode: 400,
		Method:     "POST",
		Path:       "/api/v4/secrets/batch",
		Message:    "",
	}
	if strings.Contains(err.Error(), canary) {
		t.Errorf("APIError printed a body: %v", err)
	}
	// carriesSecrets is what suppresses the message on those paths. If a new
	// endpoint is added and not listed there, this is the test that should have
	// been extended alongside it.
	for _, path := range []string{
		"/api/v1/auth/universal-auth/login",
		"/api/v4/secrets/batch",
		"/api/v4/secrets/batch/raw",
	} {
		if !carriesSecrets(path) {
			t.Errorf("carriesSecrets(%q) = false, want true", path)
		}
	}
	for _, path := range []string{"/api/v2/folders", "/api/v1/workspace/slug/p"} {
		if carriesSecrets(path) {
			t.Errorf("carriesSecrets(%q) = true, want false", path)
		}
	}
}
