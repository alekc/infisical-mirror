package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alekc/infisical-mirror/internal/config"
	"github.com/alekc/infisical-mirror/internal/state"
)

// fakeInfisical answers the two calls a plan makes: resolve a project slug,
// and list an environment's secrets.
type fakeInfisical struct {
	// secrets is keyed by "<project slug>|<environment>", each entry a
	// folder path mapped to key/value pairs.
	secrets map[string]map[string]map[string]string

	// writable registers the folder and batch endpoints. Off by default, so a
	// test that does not ask for it runs against a server that would reject a
	// write, which is what keeps the read-only tests honest about being
	// read-only rather than merely not having tried.
	writable bool
	// failPath makes any batch touching this absolute path fail.
	failPath string

	mu             sync.Mutex
	writes         []string
	foldersCreated []string
}

// recorded returns what reached the instance, under the lock.
func (f *fakeInfisical) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

func (f *fakeInfisical) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	if f.writable {
		f.enableWrites(mux)
	}

	mux.HandleFunc("GET /api/v1/projects/slug/{slug}", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		// Unwrapped, which is what both live instances return.
		writeJSON(w, map[string]any{
			"id":   "id-" + slug,
			"slug": slug,
			"name": slug,
			"environments": []map[string]string{
				{"id": "env-prod", "slug": "prod", "name": "Production"},
				{"id": "env-staging", "slug": "staging", "name": "Staging"},
			},
		})
	})

	mux.HandleFunc("GET /api/v4/secrets", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		slug := strings.TrimPrefix(q.Get("projectId"), "id-")
		root := q.Get("secretPath")
		recursive := q.Get("recursive") == "true"

		var out []map[string]any
		for folder, kv := range f.secrets[slug+"|"+q.Get("environment")] {
			if folder != root && !(recursive && strings.HasPrefix(folder, strings.TrimSuffix(root, "/")+"/")) {
				continue
			}
			for key, value := range kv {
				out = append(out, map[string]any{
					"id": key, "secretKey": key, "secretValue": value,
					"secretPath": folder, "type": "shared", "version": 1,
					"updatedAt": "2026-09-17T00:00:00Z",
				})
			}
		}
		writeJSON(w, map[string]any{"secrets": out})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// writeConfig renders a two-instance config against one fake server, which
// stands in for both sides: what matters here is the wiring, not that the two
// URLs differ.
func writeConfig(t *testing.T, url, statePath string) string {
	t.Helper()
	body := fmt.Sprintf(`
instances:
  cloud:
    url: %[1]s
    auth:
      token:
        tokenEnv: CLOUD_TOKEN
  selfhosted:
    url: %[1]s
    auth:
      token:
        tokenEnv: SELF_TOKEN

state:
  backend: file
  file:
    path: %[2]s

rules:
  - name: arr-stack
    mode: bidirectional
    a:
      instance: cloud
      project: cloud-proj
      path: /arr-stack
    b:
      instance: selfhosted
      project: self-proj
      path: /apps/arr-stack
    envMap:
      prod: prod
`, url, statePath)

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestPlanEndToEnd(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := &fakeInfisical{secrets: map[string]map[string]map[string]string{
		"cloud-proj|prod": {
			"/arr-stack":     {"API_KEY": "v1", "ONLY_ON_A": "v9"},
			"/arr-stack/sub": {"DB_URL": "v2"},
		},
		"self-proj|prod": {
			"/apps/arr-stack":     {"API_KEY": "v1"},
			"/apps/arr-stack/sub": {"DB_URL": "v2"},
		},
	}}
	url := fake.start(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfgPath := writeConfig(t, url, statePath)

	code, stdout, stderr := run(t, "plan", "--config", cfgPath)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitOK, stdout, stderr)
	}

	// The two secrets that agree across differently rooted trees are
	// converged; the one only side a has is a create.
	for _, want := range []string{
		"rule arr-stack  prod <-> prod",
		"create",
		"ONLY_ON_A",
		"2 converged",
		"first run",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output does not contain %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "v9") || strings.Contains(stdout, "v1") {
		t.Errorf("a plan printed secret values:\n%s", stdout)
	}

	// Read-only by contract: no state file, and nothing written to it.
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("plan wrote a state file (err = %v)", err)
	}
}

func TestPlanExitCodeReportsDrift(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := &fakeInfisical{secrets: map[string]map[string]map[string]string{
		"cloud-proj|prod": {"/arr-stack": {"API_KEY": "v1"}},
		"self-proj|prod":  {"/apps/arr-stack": {}},
	}}
	cfgPath := writeConfig(t, fake.start(t), filepath.Join(t.TempDir(), "state.json"))

	if code, _, _ := run(t, "plan", "--config", cfgPath); code != ExitOK {
		t.Errorf("exit = %d without --exit-code, want %d", code, ExitOK)
	}
	code, stdout, _ := run(t, "plan", "--config", cfgPath, "--exit-code")
	if code != ExitDrift {
		t.Errorf("exit = %d with --exit-code, want %d\n%s", code, ExitDrift, stdout)
	}
}

func TestPlanReportsNothingToDo(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := &fakeInfisical{secrets: map[string]map[string]map[string]string{
		"cloud-proj|prod": {"/arr-stack": {"API_KEY": "v1"}},
		"self-proj|prod":  {"/apps/arr-stack": {"API_KEY": "v1"}},
	}}
	cfgPath := writeConfig(t, fake.start(t), filepath.Join(t.TempDir(), "state.json"))

	code, stdout, stderr := run(t, "plan", "--config", cfgPath, "--exit-code")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "nothing to do") {
		t.Errorf("output does not say so:\n%s", stdout)
	}
}

func TestPlanMarksDryRun(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := &fakeInfisical{secrets: map[string]map[string]map[string]string{
		"cloud-proj|prod": {"/arr-stack": {"API_KEY": "v1"}},
		"self-proj|prod":  {"/apps/arr-stack": {"API_KEY": "v1"}},
	}}
	cfgPath := writeConfig(t, fake.start(t), filepath.Join(t.TempDir(), "state.json"))

	_, stdout, _ := run(t, "plan", "--config", cfgPath, "--dry-run")
	if !strings.Contains(stdout, "[dry run]") {
		t.Errorf("--dry-run not reflected in the output:\n%s", stdout)
	}
}

func TestPlanRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no command", nil, ExitUsage},
		{"unknown command", []string{"sync"}, ExitUsage},
		{"no config flag", []string{"plan"}, ExitUsage},
		{"missing config file", []string{"plan", "--config", "/nonexistent/config.yaml"}, ExitError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code, _, _ := run(t, c.args...); code != c.want {
				t.Errorf("exit = %d, want %d", code, c.want)
			}
		})
	}
}

func TestPlanRejectsAnUnknownRuleName(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")
	fake := &fakeInfisical{}
	cfgPath := writeConfig(t, fake.start(t), filepath.Join(t.TempDir(), "state.json"))

	// Selecting a rule that does not exist must not look like a clean run.
	code, _, stderr := run(t, "plan", "--config", cfgPath, "--rule", "typo")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "typo") {
		t.Errorf("stderr does not name the rule: %s", stderr)
	}
}

func TestPlanRequiresCredentials(t *testing.T) {
	fake := &fakeInfisical{}
	cfgPath := writeConfig(t, fake.start(t), filepath.Join(t.TempDir(), "state.json"))

	code, _, stderr := run(t, "plan", "--config", cfgPath)
	if code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "CLOUD_TOKEN") && !strings.Contains(stderr, "SELF_TOKEN") {
		t.Errorf("stderr does not name the missing variable: %s", stderr)
	}
}

func TestVersion(t *testing.T) {
	code, stdout, _ := run(t, "version")
	if code != ExitOK || strings.TrimSpace(stdout) == "" {
		t.Errorf("version: exit %d, output %q", code, stdout)
	}
}

// seedState writes a state file recording one key as synced, so that a later
// plan has something tracked to measure a vanished side against. A plan never
// writes state, so without this no guard can ever fire in an end-to-end test.
func seedState(t *testing.T, cfgPath string, keys ...string) {
	t.Helper()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("loading the config to seed state: %v", err)
	}
	pairs := cfg.Pairs()
	if len(pairs) != 1 {
		t.Fatalf("the fixture config has %d pairs, want 1", len(pairs))
	}

	store, err := state.OpenFile(cfg.State.File.Path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer store.Close()

	hasher, err := store.Hasher(t.Context())
	if err != nil {
		t.Fatalf("Hasher: %v", err)
	}

	st := state.NewRuleState()
	for _, k := range keys {
		entry := state.EntryKey("/", k)
		// The value matters: a hash that matches what the side reports is the
		// difference between "converged" and "changed on both sides".
		hv := hasher.Hash(entry, "v1", "")
		st.Put(entry, hv, hv, time.Now())
	}
	if err := store.Save(t.Context(), pairs[0].StateScope(), st); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// A side reading empty while state tracks keys is the shape of an expired
// token, a revoked permission and a genuinely emptied folder at once, and one
// of those is a mass deletion. It has to stop with its own exit code, or a
// scheduled pass reports nothing wrong while the mirror stands still.
func TestPlanExitsBlockedWhenAGuardFires(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := &fakeInfisical{secrets: map[string]map[string]map[string]string{
		"cloud-proj|prod": {"/arr-stack": {"API_KEY": "v1"}},
		// Side b answers with nothing at all.
		"self-proj|prod": {},
	}}
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfgPath := writeConfig(t, fake.start(t), statePath)
	seedState(t, cfgPath, "API_KEY")

	code, stdout, stderr := run(t, "plan", "--config", cfgPath)
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d (blocked)\nstdout:\n%s\nstderr:\n%s", code, ExitBlocked, stdout, stderr)
	}
	// The reason has to be on stdout with the plan, not only in the exit code:
	// the operator reading a CronJob's logs never sees the exit code.
	for _, want := range []string{"empty-side", "side b"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output does not contain %q:\n%s", want, stdout)
		}
	}

	// Blocked outranks drift: --exit-code must not downgrade it to 2.
	if code, _, _ := run(t, "plan", "--config", cfgPath, "--exit-code"); code != ExitBlocked {
		t.Errorf("exit = %d with --exit-code, want %d", code, ExitBlocked)
	}
}

// A plan holds a shared lock, so it must not be refused by a scheduled pass
// that happens to be reading at the same time, and it must not write.
func TestPlanDoesNotWriteStateItRead(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := &fakeInfisical{secrets: map[string]map[string]map[string]string{
		"cloud-proj|prod": {"/arr-stack": {"API_KEY": "v2"}},
		"self-proj|prod":  {"/apps/arr-stack": {"API_KEY": "v1"}},
	}}
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfgPath := writeConfig(t, fake.start(t), statePath)
	seedState(t, cfgPath, "API_KEY")

	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("reading the seeded state: %v", err)
	}

	code, stdout, stderr := run(t, "plan", "--config", cfgPath)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, "update") {
		t.Errorf("want an update for the key that changed on a:\n%s", stdout)
	}

	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("reading the state after the plan: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("plan rewrote the state file:\nbefore %s\nafter  %s", before, after)
	}
}

// Two concurrent plans are the ordinary case: an operator asking what the
// scheduled pass is about to do, while the scheduled pass is planning.
func TestTwoPlansCanRunAtOnce(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := &fakeInfisical{secrets: map[string]map[string]map[string]string{
		"cloud-proj|prod": {"/arr-stack": {"API_KEY": "v1"}},
		"self-proj|prod":  {"/apps/arr-stack": {"API_KEY": "v1"}},
	}}
	cfgPath := writeConfig(t, fake.start(t), filepath.Join(t.TempDir(), "state.json"))

	// Hold a reader open for the duration, which is what the other plan would
	// collide with if plan took an exclusive lock.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	held, err := state.OpenFile(cfg.State.File.Path, state.ReadOnly())
	if err != nil {
		t.Fatalf("holding a read lock: %v", err)
	}
	defer held.Close()

	if code, stdout, stderr := run(t, "plan", "--config", cfgPath); code != ExitOK {
		t.Fatalf("exit = %d while another reader held the file, want %d\nstdout:\n%s\nstderr:\n%s",
			code, ExitOK, stdout, stderr)
	}
}

// enableWrites adds the folder and batch endpoints, so a full apply can run
// against this fake. Registered separately from the read endpoints so that a
// test which does not call this has a server that would reject a write, which
// is how the read-only tests stay honest about being read-only.
func (f *fakeInfisical) enableWrites(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v2/folders", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		slug := strings.TrimPrefix(q.Get("projectId"), "id-")
		root := normalizeFolder(q.Get("path"))

		var out []map[string]string
		for folder := range f.secrets[slug+"|"+q.Get("environment")] {
			rel, ok := strings.CutPrefix(folder, strings.TrimSuffix(root, "/")+"/")
			if !ok || rel == "" {
				continue
			}
			name, _, _ := strings.Cut(rel, "/")
			out = append(out, map[string]string{"id": "f-" + rel, "name": name, "relativePath": "/" + rel})
		}
		writeJSON(w, map[string]any{"folders": out})
	})

	mux.HandleFunc("POST /api/v2/folders", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.foldersCreated = append(f.foldersCreated, normalizeFolder(body["path"])+"/"+body["name"])
		writeJSON(w, map[string]any{"folder": map[string]string{"id": "new"}})
	})

	batch := func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ProjectID   string `json:"projectId"`
			Environment string `json:"environment"`
			SecretPath  string `json:"secretPath"`
			Secrets     []struct {
				Key     string `json:"secretKey"`
				Value   string `json:"secretValue"`
				Comment string `json:"secretComment"`
			} `json:"secrets"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}

		f.mu.Lock()
		defer f.mu.Unlock()

		slug := strings.TrimPrefix(body.ProjectID, "id-")
		if f.failPath != "" && body.SecretPath == f.failPath {
			// A folder the server refuses, which is what the partial-failure
			// path through the CLI needs to exercise.
			http.Error(w, `{"message":"refused"}`, http.StatusUnprocessableEntity)
			return
		}

		key := slug + "|" + body.Environment
		if f.secrets[key] == nil {
			f.secrets[key] = map[string]map[string]string{}
		}
		if f.secrets[key][body.SecretPath] == nil {
			f.secrets[key][body.SecretPath] = map[string]string{}
		}
		for _, s := range body.Secrets {
			f.writes = append(f.writes, fmt.Sprintf("%s %s %s:%s", r.Method, slug, body.SecretPath, s.Key))
			if r.Method == http.MethodDelete {
				delete(f.secrets[key][body.SecretPath], s.Key)
				continue
			}
			f.secrets[key][body.SecretPath][s.Key] = s.Value
		}
		writeJSON(w, map[string]any{"secrets": []any{}})
	}
	mux.HandleFunc("POST /api/v4/secrets/batch", batch)
	mux.HandleFunc("PATCH /api/v4/secrets/batch", batch)
	mux.HandleFunc("DELETE /api/v4/secrets/batch", batch)
}

func normalizeFolder(p string) string {
	if p == "" {
		return "/"
	}
	return strings.TrimSuffix(p, "/")
}

// writableFake is the common fixture for the apply tests: side a holds two
// secrets, side b holds one of them at a different value and is missing the
// other.
func writableFake() *fakeInfisical {
	return &fakeInfisical{
		writable: true,
		secrets: map[string]map[string]map[string]string{
			"cloud-proj|prod": {
				// Distinctive values, so that asserting none of them reached
				// the output cannot pass or fail by coincidence. A one-letter
				// value matches half the words in a plan.
				"/arr-stack":     {"API_KEY": "zqx-fresh-value", "NEW_ONE": "zqx-new-value"},
				"/arr-stack/sub": {"DB_URL": "zqx-db-value"},
			},
			"self-proj|prod": {
				"/apps/arr-stack": {"API_KEY": "zqx-stale-value"},
			},
		},
	}
}

func TestApplyEndToEnd(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := writableFake()
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfgPath := writeConfig(t, fake.start(t), statePath)

	code, stdout, stderr := run(t, "apply", "--config", cfgPath)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitOK, stdout, stderr)
	}

	// Two creates on b (NEW_ONE at the root, DB_URL in the subfolder). API_KEY
	// differs on both sides with no state, so newest-wins has equal timestamps
	// and reports a conflict rather than picking.
	writes := fake.recorded()
	if len(writes) != 2 {
		t.Fatalf("wrote %d secret(s), want 2: %v", len(writes), writes)
	}
	if !strings.Contains(stdout, "2 created") {
		t.Errorf("output does not report what landed:\n%s", stdout)
	}

	// Values never appear, the same as in a plan.
	if strings.Contains(stdout, "zqx-") {
		t.Errorf("apply printed a secret value:\n%s", stdout)
	}

	// The state records what was written, so the next pass is a no-op for it.
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("apply did not write the state: %v", err)
	}
	code, stdout, _ = run(t, "plan", "--config", cfgPath, "--exit-code")
	if strings.Count(stdout, "create") != 0 {
		t.Errorf("the second pass still wants to create something:\n%s", stdout)
	}
	// The fixture also carries a conflict that no policy resolves, and apply
	// leaves it exactly where it was rather than picking a side, so the second
	// pass is still pending: exit 2 under --exit-code. A 0 here would mean the
	// conflict had been quietly decided by the apply above.
	if code != 2 {
		t.Errorf("the second pass exited %d, want 2 with the conflict still open:\n%s", code, stdout)
	}
	if strings.Count(stdout, "conflict  ") != 1 {
		t.Errorf("want exactly the one untouched conflict left:\n%s", stdout)
	}
}

// TestApplyDryRunWritesNothingAnywhere is the safety property that makes the
// shadow phase possible: --dry-run has to stop both the instance write and the
// state write, because a state that records a sync which never happened makes
// the next real run believe both sides already agree.
func TestApplyDryRunWritesNothingAnywhere(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := writableFake()
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfgPath := writeConfig(t, fake.start(t), statePath)

	code, stdout, stderr := run(t, "apply", "--config", cfgPath, "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitOK, stdout, stderr)
	}
	if writes := fake.recorded(); len(writes) != 0 {
		t.Fatalf("a dry-run apply wrote to the instance: %v", writes)
	}
	if len(fake.foldersCreated) != 0 {
		t.Fatalf("a dry-run apply created folders: %v", fake.foldersCreated)
	}
	if !strings.Contains(stdout, "dry run") {
		t.Errorf("the output does not say it was a dry run:\n%s", stdout)
	}

	// The state file is created by opening the store, but it must hold no
	// entries for this rule.
	store, err := state.OpenFile(statePath, state.ReadOnly())
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer store.Close()
	if scopes := store.Scopes(); len(scopes) != 0 {
		t.Errorf("a dry run recorded state for %v", scopes)
	}
}

func TestApplyCreatesTheDestinationFolder(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := writableFake()
	cfgPath := writeConfig(t, fake.start(t), filepath.Join(t.TempDir(), "state.json"))

	if code, stdout, stderr := run(t, "apply", "--config", cfgPath); code != ExitOK {
		t.Fatalf("exit = %d\n%s%s", code, stdout, stderr)
	}

	// /apps/arr-stack/sub does not exist on side b, and Infisical does not
	// create a destination folder implicitly: a write into a missing folder
	// fails. This is the caller the folder code has been waiting for since
	// phase 2.
	if !slices.Contains(fake.foldersCreated, "/apps/arr-stack/sub") {
		t.Errorf("the missing destination folder was not created: %v", fake.foldersCreated)
	}
}

// TestApplyKeepsGoingAfterAFailedFolder checks the whole partial-failure path
// through the CLI, not just through Apply: one folder refuses, the other still
// lands, and the state records only what landed.
func TestApplyKeepsGoingAfterAFailedFolder(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := writableFake()
	fake.failPath = "/apps/arr-stack/sub"
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfgPath := writeConfig(t, fake.start(t), statePath)

	code, stdout, stderr := run(t, "apply", "--config", cfgPath)
	if code != ExitError {
		t.Fatalf("exit = %d, want %d for a run with a failed batch\n%s%s", code, ExitError, stdout, stderr)
	}
	if !strings.Contains(stdout, "1 created") || !strings.Contains(stdout, "1 failed") {
		t.Errorf("output does not separate what landed from what did not:\n%s", stdout)
	}

	// The second pass must still want to write the one that failed, which is
	// only true if its state entry was reverted.
	_, stdout, _ = run(t, "plan", "--config", cfgPath)
	if !strings.Contains(stdout, "DB_URL") {
		t.Errorf("the failed key is not pending on the next pass, so it would never be retried:\n%s", stdout)
	}
}

func TestApplyRefusesWhenAGuardFires(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	// Four tracked keys, all changed on side a and none on side b, so all four
	// are overwrites: 4 of 4, far over the 25% default and over the floor of
	// three. The empty-side guard is not used, because an emptied side produces
	// conflicts rather than writes and would prove nothing about --force.
	fake := &fakeInfisical{
		writable: true,
		secrets: map[string]map[string]map[string]string{
			"cloud-proj|prod": {"/arr-stack": {"A": "changed", "B": "changed", "C": "changed", "D": "changed"}},
			"self-proj|prod":  {"/apps/arr-stack": {"A": "v1", "B": "v1", "C": "v1", "D": "v1"}},
		},
	}
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfgPath := writeConfig(t, fake.start(t), statePath)
	seedState(t, cfgPath, "A", "B", "C", "D")

	code, stdout, stderr := run(t, "apply", "--config", cfgPath)
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitBlocked, stdout, stderr)
	}
	if writes := fake.recorded(); len(writes) != 0 {
		t.Fatalf("a refused rule wrote to the instance: %v", writes)
	}
	if !strings.Contains(stdout, "REFUSED") {
		t.Errorf("the plan output does not show the refusal:\n%s", stdout)
	}

	// --force is the documented way past it, and it has to actually write.
	code, stdout, stderr = run(t, "apply", "--config", cfgPath, "--force")
	if code != ExitOK {
		t.Fatalf("forced exit = %d, want %d\n%s%s", code, ExitOK, stdout, stderr)
	}
	if writes := fake.recorded(); len(writes) == 0 {
		t.Fatal("--force did not carry the plan out")
	}
	if !strings.Contains(stderr, "--force") {
		t.Errorf("a forced run does not say so on stderr:\n%s", stderr)
	}
}

func TestStateShowSummarisesWithoutLeaking(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := writableFake()
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfgPath := writeConfig(t, fake.start(t), statePath)
	if code, _, stderr := run(t, "apply", "--config", cfgPath); code != ExitOK {
		t.Fatalf("seeding apply failed: %s", stderr)
	}

	code, stdout, stderr := run(t, "state", "show", "--config", cfgPath)
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s%s", code, stdout, stderr)
	}
	for _, want := range []string{"SCOPE", "LIVE", "TOMBSTONED", "fingerprint"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output is missing %q:\n%s", want, stdout)
		}
	}
	// The salt itself never appears, only a truncated one-way function of it.
	body, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	var doc struct {
		Salt            string `json:"salt"`
		SaltFingerprint string `json:"saltFingerprint"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	if doc.Salt != "" && strings.Contains(stdout, doc.Salt) {
		t.Error("state show printed the salt")
	}
	if strings.Contains(stdout, doc.SaltFingerprint) {
		t.Error("state show printed the whole fingerprint rather than a short form")
	}
}

func TestStateShowOneScopeTruncatesHashes(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := writableFake()
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfgPath := writeConfig(t, fake.start(t), statePath)
	if code, _, stderr := run(t, "apply", "--config", cfgPath); code != ExitOK {
		t.Fatalf("seeding apply failed: %s", stderr)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	scope := cfg.Pairs()[0].StateScope()

	code, detail, stderr := run(t, "state", "show", "--config", cfgPath, "--scope", scope)
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s%s", code, detail, stderr)
	}
	if !strings.Contains(detail, "NEW_ONE") {
		t.Errorf("the scope listing does not name its keys:\n%s", detail)
	}

	// Every hash in the listing is the short form. A full keyed hash is an
	// equality oracle for anyone who can compute one, and there is no reason
	// to print a document's worth of them to a terminal or a CI log.
	for _, field := range strings.Fields(detail) {
		if len(field) == 64 && !strings.ContainsAny(field, "/:-") {
			t.Errorf("a full-length hash reached the output: %s", field)
		}
	}
}

func TestStateShowRejectsAnUnknownScope(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := writableFake()
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfgPath := writeConfig(t, fake.start(t), statePath)
	if code, _, stderr := run(t, "apply", "--config", cfgPath); code != ExitOK {
		t.Fatalf("seeding apply failed: %s", stderr)
	}

	// A typo must not read as an empty scope: "this rule has never synced" and
	// "you named a scope that does not exist" need different answers.
	code, _, stderr := run(t, "state", "show", "--config", cfgPath, "--scope", "no-such-scope")
	if code == ExitOK {
		t.Fatal("an unknown scope was reported as an empty one")
	}
	if !strings.Contains(stderr, "no scope named") {
		t.Errorf("the error does not say what went wrong:\n%s", stderr)
	}
}

func TestStatePruneDropsOnlyOldTombstones(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := writableFake()
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfgPath := writeConfig(t, fake.start(t), statePath)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	scope := cfg.Pairs()[0].StateScope()

	store, err := state.OpenFile(statePath)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	st := state.NewRuleState()
	st.Tombstone("/|OLD", time.Now().Add(-60*24*time.Hour))
	st.Tombstone("/|RECENT", time.Now().Add(-time.Hour))
	st.Put("/|LIVE", "a", "b", time.Now())
	if err := store.Save(t.Context(), scope, st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	store.Close()

	// A dry run reports and writes nothing.
	code, stdout, stderr := run(t, "state", "prune", "--config", cfgPath, "--dry-run")
	if code != ExitOK {
		t.Fatalf("dry-run exit = %d\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "nothing was written") {
		t.Errorf("the dry run does not say it wrote nothing:\n%s", stdout)
	}

	reopened, err := state.OpenFile(statePath, state.ReadOnly())
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	after, _ := reopened.Load(t.Context(), scope)
	reopened.Close()
	if len(after.Entries) != 3 {
		t.Fatalf("the dry run changed the file: %d entries, want 3", len(after.Entries))
	}

	// The real run drops the old tombstone and nothing else.
	if code, stdout, stderr := run(t, "state", "prune", "--config", cfgPath); code != ExitOK {
		t.Fatalf("exit = %d\n%s%s", code, stdout, stderr)
	}
	reopened, err = state.OpenFile(statePath, state.ReadOnly())
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer reopened.Close()
	after, _ = reopened.Load(t.Context(), scope)

	if _, ok := after.Get("/|OLD"); ok {
		t.Error("the old tombstone survived")
	}
	for _, key := range []string{"/|RECENT", "/|LIVE"} {
		if _, ok := after.Get(key); !ok {
			t.Errorf("%s was dropped; pruning a recent tombstone resurrects a deletion somebody meant", key)
		}
	}
}

func TestStatePruneRefusesANonPositiveAge(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	cfgPath := writeConfig(t, writableFake().start(t), filepath.Join(t.TempDir(), "state.json"))
	// Zero would drop the tombstone the pass that just ran wrote, which is the
	// exact case tombstones exist for.
	if code, _, _ := run(t, "state", "prune", "--config", cfgPath, "--older-than", "0"); code != ExitUsage {
		t.Errorf("--older-than 0 exit = %d, want %d", code, ExitUsage)
	}
}

func TestVersionIsReportedByBothSpellings(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		code, stdout, stderr := run(t, args...)
		if code != ExitOK {
			t.Fatalf("%v exit = %d\n%s%s", args, code, stdout, stderr)
		}
		// Both spellings go through one function, so they cannot report
		// different things.
		for _, want := range []string{"infisical-mirror", Version, Commit, BuildDate} {
			if !strings.Contains(stdout, want) {
				t.Errorf("%v output %q is missing %q", args, strings.TrimSpace(stdout), want)
			}
		}
	}
}

// TestResolveBuildPrefersLdflagsThenEmbeddedInfo pins the precedence, because
// the failure it guards against is silent: a release binary that reports its
// tag from the wrong source still prints something plausible.
func TestResolveBuildPrefersLdflagsThenEmbeddedInfo(t *testing.T) {
	settings := func(kv ...string) []debug.BuildSetting {
		out := make([]debug.BuildSetting, 0, len(kv)/2)
		for i := 0; i < len(kv); i += 2 {
			out = append(out, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return out
	}

	tests := []struct {
		name                          string
		version, commit, date         string
		bi                            *debug.BuildInfo
		ok                            bool
		wantVersion, wantCommit, want string
	}{{
		name:    "ldflags win over embedded info",
		version: "0.1.1", commit: "abc1234", date: "2026-09-20T00:00:00Z",
		bi: &debug.BuildInfo{
			Main:     debug.Module{Version: "v9.9.9"},
			Settings: settings("vcs.revision", "ffffffffffffffffffffffffffffffffffffffff", "vcs.time", "2000-01-01T00:00:00Z"),
		},
		ok:          true,
		wantVersion: "0.1.1", wantCommit: "abc1234", want: "2026-09-20T00:00:00Z",
	}, {
		// The `go install path@v0.1.1` shape: a proxy build carries the module
		// version and no vcs.* settings whatsoever.
		name:    "go install recovers the tag and leaves the commit alone",
		version: "dev", commit: "none", date: "unknown",
		bi:          &debug.BuildInfo{Main: debug.Module{Version: "v0.1.1"}},
		ok:          true,
		wantVersion: "0.1.1", wantCommit: "none", want: "unknown",
	}, {
		// The `go build` shape: vcs.* present, and a synthesised pseudo-version
		// that must not be reported, because it names an unreleased 0.1.2.
		name:    "go build recovers the commit and rejects the pseudo-version",
		version: "dev", commit: "none", date: "unknown",
		bi: &debug.BuildInfo{
			Main:     debug.Module{Version: "v0.1.2-0.20260920170255-3e60addac030"},
			Settings: settings("vcs.revision", "0be0fc9aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "vcs.time", "2026-09-19T12:00:00Z", "vcs.modified", "false"),
		},
		ok:          true,
		wantVersion: "dev", wantCommit: "0be0fc9", want: "2026-09-19T12:00:00Z",
	}, {
		// Older toolchains used this placeholder instead of a pseudo-version.
		name:    "the devel placeholder is rejected too",
		version: "dev", commit: "none", date: "unknown",
		bi:          &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
		ok:          true,
		wantVersion: "dev", wantCommit: "none", want: "unknown",
	}, {
		// A real tag on a modified tree. The tag is true of the commit and not
		// of the binary, so reporting it bare would be the misleading case.
		name:    "a tagged build from a dirty tree reports neither a bare tag nor a clean commit",
		version: "dev", commit: "none", date: "unknown",
		bi: &debug.BuildInfo{
			Main:     debug.Module{Version: "v0.1.1+dirty"},
			Settings: settings("vcs.revision", "0be0fc9aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "vcs.modified", "true"),
		},
		ok:          true,
		wantVersion: "dev", wantCommit: "0be0fc9-dirty", want: "unknown",
	}, {
		name:    "no build info at all leaves every default in place",
		version: "dev", commit: "none", date: "unknown",
		bi:          nil,
		ok:          false,
		wantVersion: "dev", wantCommit: "none", want: "unknown",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			version, commit, date := resolveBuild(tc.version, tc.commit, tc.date, tc.bi, tc.ok)
			if version != tc.wantVersion || commit != tc.wantCommit || date != tc.want {
				t.Errorf("resolveBuild = (%q, %q, %q), want (%q, %q, %q)",
					version, commit, date, tc.wantVersion, tc.wantCommit, tc.want)
			}
		})
	}
}

// TestDaemonServesMetricsAndKeepsReconciling exercises the mode that exists
// because a scrape target has to be alive when Prometheus arrives.
func TestDaemonServesMetricsAndKeepsReconciling(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := writableFake()
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfgPath := writeConfig(t, fake.start(t), statePath)

	// A free port, so a developer machine with something on 9090 does not fail
	// this. The listener is closed straight away and the address reused, which
	// is a race in principle and has never been one in practice on a loopback
	// ephemeral port.
	addr := freeAddr(t)
	appendToFile(t, cfgPath, "\ndaemon:\n  interval: 1m\nmetrics:\n  listen: \""+addr+"\"\n")

	ctx, cancel := context.WithCancel(t.Context())
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- Run(ctx, []string{"plan", "--config", cfgPath, "--daemon"}, &stdout, &stderr) }()

	body := pollMetrics(t, "http://"+addr+"/metrics")
	cancel()

	select {
	case code := <-done:
		if code != ExitOK {
			t.Errorf("the daemon exited %d on cancellation, want %d\n%s%s", code, ExitOK, stdout.String(), stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not stop when its context was cancelled")
	}

	for _, want := range []string{
		"infisical_mirror_build_info",
		"infisical_mirror_last_run_timestamp_seconds",
		`rule="arr-stack"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the endpoint is missing %q:\n%s", want, body)
		}
	}
	// A daemon planning is still a plan: it must not have written state.
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("a planning daemon wrote a state file (err = %v)", err)
	}
}

func TestOneShotWritesTheTextfileWhenConfigured(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	fake := writableFake()
	cfgPath := writeConfig(t, fake.start(t), filepath.Join(t.TempDir(), "state.json"))
	promPath := filepath.Join(t.TempDir(), "infisical_mirror.prom")
	appendToFile(t, cfgPath, "\nmetrics:\n  textfile: "+promPath+"\n")

	if code, stdout, stderr := run(t, "plan", "--config", cfgPath); code != ExitOK {
		t.Fatalf("exit = %d\n%s%s", code, stdout, stderr)
	}

	body, err := os.ReadFile(promPath)
	if err != nil {
		t.Fatalf("the textfile was not written: %v", err)
	}
	if !strings.Contains(string(body), `rule="arr-stack"`) {
		t.Errorf("the textfile does not carry the rule:\n%s", body)
	}
	// Same rule as the endpoint: counts, never key names or values. The values
	// share a distinctive prefix so this cannot match a word in a HELP string,
	// which is how the first version of this assertion failed: the help on
	// last_success_timestamp_seconds contains the word "stale".
	for _, forbidden := range []string{"API_KEY", "NEW_ONE", "DB_URL", "zqx-"} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("the textfile contains %q", forbidden)
		}
	}
}

func TestOneShotWritesNoTextfileByDefault(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")

	dir := t.TempDir()
	cfgPath := writeConfig(t, writableFake().start(t), filepath.Join(dir, "state.json"))
	if code, _, stderr := run(t, "plan", "--config", cfgPath); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".prom") {
			t.Errorf("a textfile was written without being asked for: %s", e.Name())
		}
	}
}

func appendToFile(t *testing.T, path, extra string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(extra); err != nil {
		t.Fatalf("appending to %s: %v", path, err)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// pollMetrics waits for the endpoint to come up and returns its body. The
// daemon binds and runs its first pass concurrently, so a single immediate
// request would race the listener.
func pollMetrics(t *testing.T, url string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err != nil {
			last = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("reading /metrics: %v", err)
		}
		// Wait for the first pass to have recorded something, not merely for
		// the listener: an empty exposition would pass every assertion below
		// for the wrong reason.
		if strings.Contains(string(body), "infisical_mirror_last_run_timestamp_seconds") {
			return string(body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the metrics endpoint never reported a completed pass (last error: %v)", last)
	return ""
}
