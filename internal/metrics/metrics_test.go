package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r := New()
	r.SetBuildInfo("1.2.3", "abc1234", "2026-09-19")
	return r
}

// render gathers the registry into the textfile exposition, which is what both
// sinks emit, so asserting on it covers the endpoint too.
func render(t *testing.T, r *Registry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.prom")
	if err := r.WriteTextfile(path); err != nil {
		t.Fatalf("WriteTextfile: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	return string(body)
}

// TestNoSecretMaterialReachesALabel is the test this package exists to make
// possible. A label is stored forever, replicated to every federation target,
// and rendered on dashboards by people with no business reading the key
// names. See docs/design.md.
func TestNoSecretMaterialReachesALabel(t *testing.T) {
	r := testRegistry(t)

	id := RuleID{Rule: "home-cluster", EnvA: "prod", EnvB: "prod"}
	r.BeginPass()
	r.ObservePlan(id, PlanCounts{
		SecretsA: 77, SecretsB: 42,
		Actions:   map[string]int{"create": 36, "update": 6},
		Converged: 35, Tracked: 40,
	})
	r.ObserveApply(id, map[string]int{"create": 36}, 0)
	r.EndPass("apply", time.Now(), true)
	r.ObservePassStarted("webhook")
	r.ObserveWebhook("cloud", "accepted")
	r.ObserveWebhook("", "unknown_source")

	body := render(t, r)

	// Nothing that could only have come from a secret, a key name or a folder
	// path may appear anywhere in the exposition.
	for _, forbidden := range []string{
		"API_KEY", "PASSWORD", "s3cret", "/samba", "/telegram", "amneziawg",
		"secretKey", "secretValue", "clientSecret",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the exposition contains %q; labels carry counts, never key names, paths or values", forbidden)
		}
	}

	// And positively: the whole label vocabulary is the three names plus the
	// two enumerations. A new label showing up here is a deliberate decision,
	// and this is where it gets made.
	allowed := map[string]bool{
		"rule": true, "env_a": true, "env_b": true, "op": true, "side": true,
		"command": true, "result": true,
		"version": true, "commit": true, "date": true,
		"trigger": true, "source": true, "outcome": true,
	}
	for _, name := range labelNames(body) {
		if !allowed[name] {
			t.Errorf("unexpected label %q in the exposition; see the package comment on what a label costs", name)
		}
	}
}

// labelNames extracts every label name used in a textfile exposition.
func labelNames(body string) []string {
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		open := strings.Index(line, "{")
		end := strings.LastIndex(line, "}")
		if open < 0 || end < open {
			continue
		}
		for _, pair := range strings.Split(line[open+1:end], `",`) {
			name, _, ok := strings.Cut(pair, "=")
			if !ok {
				continue
			}
			name = strings.TrimSpace(name)
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	return names
}

// TestBeginPassClearsStaleRuleGauges covers the failure that makes a dashboard
// lie: a rule removed from the config keeps reporting its last values forever,
// and a stale gauge reads as a rule that is fine.
func TestBeginPassClearsStaleRuleGauges(t *testing.T) {
	r := testRegistry(t)

	old := RuleID{Rule: "retired-rule", EnvA: "prod", EnvB: "prod"}
	r.BeginPass()
	r.ObservePlan(old, PlanCounts{SecretsA: 5, SecretsB: 5, Actions: map[string]int{"create": 1}})
	if !strings.Contains(render(t, r), "retired-rule") {
		t.Fatal("the fixture never recorded the rule, so this test proves nothing")
	}

	// A later pass no longer sees that rule.
	r.BeginPass()
	r.ObservePlan(RuleID{Rule: "live-rule", EnvA: "prod", EnvB: "prod"}, PlanCounts{SecretsA: 1, SecretsB: 1})

	body := render(t, r)
	if strings.Contains(body, "retired-rule") {
		t.Error("a rule that no longer exists is still reporting its last values, which reads as a rule that is fine")
	}
	if !strings.Contains(body, "live-rule") {
		t.Error("the current rule is missing from the exposition")
	}
}

// TestCountersSurviveAPass is the other half: resetting a counter makes every
// rate() over it misread, because a counter going backwards is read as a
// process restart.
func TestCountersSurviveAPass(t *testing.T) {
	r := testRegistry(t)
	id := RuleID{Rule: "r", EnvA: "prod", EnvB: "prod"}

	for range 3 {
		r.BeginPass()
		r.ObserveApply(id, map[string]int{"create": 2}, 0)
		r.EndPass("apply", time.Now(), true)
	}

	body := render(t, r)
	if !strings.Contains(body, `infisical_mirror_applied_total{env_a="prod",env_b="prod",op="create",rule="r"} 6`) {
		t.Errorf("applied_total did not accumulate across passes; body:\n%s", body)
	}
}

// TestSuccessTimestampOnlyMovesOnSuccess is what a staleness alert depends on.
// If a failed pass updated it, the alert would never fire on a mirror that has
// been failing every pass for a week.
func TestSuccessTimestampOnlyMovesOnSuccess(t *testing.T) {
	r := testRegistry(t)

	r.EndPass("plan", time.Now(), false)
	if strings.Contains(render(t, r), "infisical_mirror_last_success_timestamp_seconds") {
		t.Fatal("a failed pass set the success timestamp, so a staleness alert would never fire")
	}
	if !strings.Contains(render(t, r), "infisical_mirror_last_run_timestamp_seconds") {
		t.Fatal("a failed pass did not set the run timestamp either, so nothing records that it ran")
	}

	r.EndPass("plan", time.Now(), true)
	if !strings.Contains(render(t, r), "infisical_mirror_last_success_timestamp_seconds") {
		t.Fatal("a successful pass did not set the success timestamp")
	}
}

// TestZeroedOpsAreStillReported covers the difference between a gauge that
// reads zero and one that is absent. On a graph they look the same and mean
// opposite things: "nothing to delete" versus "this rule stopped reporting".
func TestZeroedOpsAreStillReported(t *testing.T) {
	r := testRegistry(t)
	r.BeginPass()
	r.ObservePlan(RuleID{Rule: "r", EnvA: "prod", EnvB: "prod"}, PlanCounts{
		Actions: map[string]int{"create": 0, "update": 0, "delete": 0, "conflict": 0},
	})

	body := render(t, r)
	for _, op := range []string{"create", "update", "delete", "conflict"} {
		if !strings.Contains(body, `op="`+op+`"`) {
			t.Errorf("op %q is absent rather than zero; an absent series and a zero one read the same and mean opposite things", op)
		}
	}
}

func TestWriteTextfileIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.prom")

	r := testRegistry(t)
	r.BeginPass()
	r.ObservePlan(RuleID{Rule: "first", EnvA: "prod", EnvB: "prod"}, PlanCounts{SecretsA: 1})
	if err := r.WriteTextfile(path); err != nil {
		t.Fatalf("WriteTextfile: %v", err)
	}

	r.BeginPass()
	r.ObservePlan(RuleID{Rule: "second", EnvA: "prod", EnvB: "prod"}, PlanCounts{SecretsA: 2})
	if err := r.WriteTextfile(path); err != nil {
		t.Fatalf("WriteTextfile, second: %v", err)
	}

	// No temp file survives either write. A collector scrapes a whole
	// directory, so a leftover temp file is a second, stale copy of every
	// series, which a scrape reports as a duplicate rather than ignoring.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "m.prom" {
			t.Errorf("left %s behind; a collector reads the whole directory", e.Name())
		}
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if strings.Contains(string(body), "first") {
		t.Error("the file still carries the previous pass; rename should have replaced it wholesale")
	}
}

func TestWriteTextfileIsReadableByTheCollector(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.prom")
	if err := testRegistry(t).WriteTextfile(path); err != nil {
		t.Fatalf("WriteTextfile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// The collector runs as another user. The exposition holds counts, never
	// keys or values, so this is deliberate rather than an oversight.
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode is %o, want 644: the collector runs as a different user and would not be able to read it", perm)
	}
}

func TestWriteTextfileReportsAnUnwritableDirectory(t *testing.T) {
	// A CronJob pointed at a path its volume mount does not provide is the
	// ordinary way this fails, and it must say so rather than silently
	// reporting nothing.
	err := testRegistry(t).WriteTextfile(filepath.Join(t.TempDir(), "no-such-dir", "m.prom"))
	if err == nil {
		t.Fatal("writing into a directory that does not exist reported success")
	}
}

func TestBuildInfoCarriesTheBuild(t *testing.T) {
	body := render(t, testRegistry(t))
	if !strings.Contains(body, `version="1.2.3"`) || !strings.Contains(body, `commit="abc1234"`) {
		t.Errorf("build_info does not carry the build; body:\n%s", body)
	}
}
