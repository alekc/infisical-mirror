package config

import (
	"strings"
	"testing"
)

// base is a minimal valid document. Tests build on it rather than repeating it,
// so a test that fails points at the one thing it changed.
const base = `
instances:
  cloud:
    url: https://app.infisical.com
    auth:
      universalAuth:
        clientIdEnv: CLOUD_CLIENT_ID
        clientSecretEnv: CLOUD_CLIENT_SECRET
  selfhosted:
    url: https://secret.alekc.dev
    auth:
      universalAuth:
        clientIdEnv: SELF_CLIENT_ID
        clientSecretEnv: SELF_CLIENT_SECRET
state:
  backend: file
  file:
    path: /var/lib/infisical-mirror/state.json
rules:
  - name: home-cluster
    mode: bidirectional
    a: {instance: cloud, project: alekc-home-cluster, path: /}
    b: {instance: selfhosted, project: home-cluster-k-wv9, path: /}
    envMap:
      prod: prod
`

func parse(t *testing.T, doc string) (*Config, error) {
	t.Helper()
	return Parse(strings.NewReader(doc))
}

func mustParse(t *testing.T, doc string) *Config {
	t.Helper()
	cfg, err := parse(t, doc)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return cfg
}

func TestParseFillsDefaults(t *testing.T) {
	cfg := mustParse(t, base)

	if got := cfg.Defaults.Conflict; got != DefaultConflict {
		t.Errorf("defaults.conflict = %q, want %q", got, DefaultConflict)
	}
	if got := cfg.Defaults.Delete; got != DeleteIgnore {
		t.Errorf("defaults.delete = %q, want %q", got, DeleteIgnore)
	}
	if got := *cfg.Defaults.MaxChangeRatio; got != DefaultMaxChangeRatio {
		t.Errorf("defaults.maxChangeRatio = %v, want %v", got, DefaultMaxChangeRatio)
	}
	if got := *cfg.Rules[0].Recursive; got != true {
		t.Errorf("rule recursive = %v, want true", got)
	}
	if got := cfg.Rules[0].Delete; got != DeleteIgnore {
		t.Errorf("rule delete = %q, want %q (deletions are opt-in)", got, DeleteIgnore)
	}
}

func TestAuthMethods(t *testing.T) {
	tests := []struct {
		name string
		auth string
		want string // substring of the expected error, empty when it should pass
	}{
		{
			name: "universal auth",
			auth: "      universalAuth:\n        clientIdEnv: ID\n        clientSecretEnv: SECRET\n",
		},
		{
			name: "token",
			auth: "      token:\n        tokenEnv: TOKEN\n",
		},
		{
			name: "both",
			auth: "      token:\n        tokenEnv: TOKEN\n      universalAuth:\n        clientIdEnv: ID\n        clientSecretEnv: SECRET\n",
			want: "mutually exclusive",
		},
		{
			name: "neither",
			auth: "      token:\n",
			want: "one of universalAuth or token is required",
		},
		{
			name: "token without the env var name",
			auth: "      token:\n        tokenEnv: \"\"\n",
			want: "tokenEnv: required",
		},
		{
			name: "universal auth missing the secret",
			auth: "      universalAuth:\n        clientIdEnv: ID\n",
			want: "clientSecretEnv: required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := strings.Replace(base,
				"      universalAuth:\n        clientIdEnv: SELF_CLIENT_ID\n        clientSecretEnv: SELF_CLIENT_SECRET\n",
				tt.auth, 1)

			cfg, err := parse(t, doc)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("Parse() error = %v, want it accepted", err)
			case tt.want == "":
				if cfg.Instances["selfhosted"].Auth.Token == nil && cfg.Instances["selfhosted"].Auth.UniversalAuth == nil {
					t.Error("neither auth method survived parsing")
				}
			case err == nil:
				t.Fatalf("Parse() = nil error, want one mentioning %q", tt.want)
			case !strings.Contains(err.Error(), tt.want):
				t.Errorf("Parse() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestDryRun(t *testing.T) {
	t.Run("defaults to off", func(t *testing.T) {
		if got := mustParse(t, base).Pairs()[0].DryRun; got {
			t.Error("DryRun = true by default, want a config that says nothing to apply changes")
		}
	})

	t.Run("set per rule", func(t *testing.T) {
		doc := strings.Replace(base, "    mode: bidirectional", "    mode: bidirectional\n    dryRun: true", 1)
		if got := mustParse(t, doc).Pairs()[0].DryRun; !got {
			t.Error("DryRun = false, want the rule's own setting to win")
		}
	})

	t.Run("inherited from defaults", func(t *testing.T) {
		doc := base + "\ndefaults:\n  dryRun: true\n"
		if got := mustParse(t, doc).Pairs()[0].DryRun; !got {
			t.Error("DryRun = false, want it inherited from defaults")
		}
	})

	t.Run("force overrides a rule that opted out", func(t *testing.T) {
		doc := strings.Replace(base, "    mode: bidirectional", "    mode: bidirectional\n    dryRun: false", 1)
		cfg := mustParse(t, doc)
		cfg.ForceDryRun()

		for _, p := range cfg.Pairs() {
			if !p.DryRun {
				t.Errorf("pair %s has DryRun = false after ForceDryRun, want the flag to win", p.Rule)
			}
		}
	})
}

func TestRulesNamed(t *testing.T) {
	doc := base + `  - name: arr-stack
    mode: a-to-b
    a: {instance: cloud, project: other-project, path: /arr-stack}
    b: {instance: selfhosted, project: other-project, path: /apps/arr-stack}
    envMap:
      prod: prod
      dev: dev
`
	cfg := mustParse(t, doc)

	if got := len(cfg.Pairs()); got != 3 {
		t.Fatalf("Pairs() = %d, want 3 (one env on the first rule, two on the second)", got)
	}

	selected, err := cfg.RulesNamed("arr-stack")
	if err != nil {
		t.Fatalf("RulesNamed() error = %v", err)
	}
	if len(selected) != 2 {
		t.Errorf("RulesNamed(arr-stack) = %d pairs, want 2", len(selected))
	}
	for _, p := range selected {
		if p.Rule != "arr-stack" {
			t.Errorf("RulesNamed(arr-stack) returned a pair from rule %q", p.Rule)
		}
	}

	if all, err := cfg.RulesNamed(); err != nil || len(all) != 3 {
		t.Errorf("RulesNamed() with no names = %d pairs, err = %v, want all 3", len(all), err)
	}

	if _, err := cfg.RulesNamed("nope"); err == nil {
		t.Error("RulesNamed(nope) = nil error, want an unknown rule name to fail rather than run nothing")
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "unknown top-level field",
			doc:  base + "\nnotAField: true\n",
			want: "notAField",
		},
		{
			name: "unknown field inside a rule",
			doc:  strings.Replace(base, "    mode: bidirectional", "    mode: bidirectional\n    direction: both", 1),
			want: "direction",
		},
		{
			name: "unknown field inside an env mapping",
			doc:  strings.Replace(base, "      prod: prod", "      prod:\n        env: prod\n        cPath: /nope", 1),
			want: "cPath",
		},
		{
			name: "unknown instance reference",
			doc:  strings.Replace(base, "instance: selfhosted", "instance: typo", 1),
			want: "not declared under instances",
		},
		{
			name: "bad mode",
			doc:  strings.Replace(base, "mode: bidirectional", "mode: both-ways", 1),
			want: "is not one of",
		},
		{
			name: "relative path",
			doc:  strings.Replace(base, "path: /}", "path: arr-stack}", 1),
			want: "must start with /",
		},
		{
			name: "path escaping the root",
			doc:  strings.Replace(base, "path: /}", "path: /../other}", 1),
			want: "escapes the root",
		},
		{
			name: "empty env map",
			doc:  strings.Replace(base, "    envMap:\n      prod: prod\n", "    envMap: {}\n", 1),
			want: "at least one environment mapping",
		},
		{
			name: "ratio out of range",
			doc:  base + "\ndefaults:\n  maxChangeRatio: 1.5\n",
			want: "maxChangeRatio",
		},
		{
			name: "invalid glob",
			doc:  strings.Replace(base, "    envMap:", "    include: [\"[\"]\n    envMap:", 1),
			want: "not a valid glob",
		},
		{
			name: "no rules",
			doc:  strings.Split(base, "rules:")[0],
			want: "at least one rule",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse(t, tt.doc)
			if err == nil {
				t.Fatalf("Parse() = nil error, want one mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	doc := strings.NewReplacer(
		"mode: bidirectional", "mode: sideways",
		"instance: selfhosted", "instance: typo",
	).Replace(base)

	_, err := parse(t, doc)
	if err == nil {
		t.Fatal("Parse() = nil error, want several")
	}
	for _, want := range []string{"sideways", "typo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q as well", err, want)
		}
	}
}

func TestEnvMapShorthandAndLongForm(t *testing.T) {
	doc := strings.Replace(base, "      prod: prod\n", `      prod: prod
      dev:
        env: staging
        aPath: /arr-stack-dev
        bPath: /apps/arr-stack
`, 1)

	cfg := mustParse(t, doc)
	pairs := cfg.Pairs()
	if len(pairs) != 2 {
		t.Fatalf("Pairs() = %d pairs, want 2", len(pairs))
	}

	byEnv := map[string]Pair{}
	for _, p := range pairs {
		byEnv[p.A.Env] = p
	}

	prod := byEnv["prod"]
	if prod.B.Env != "prod" || prod.A.Path != "/" || prod.B.Path != "/" {
		t.Errorf("prod pair = %+v, want the rule roots and env prod", prod)
	}

	dev := byEnv["dev"]
	if dev.B.Env != "staging" {
		t.Errorf("dev pair B env = %q, want staging", dev.B.Env)
	}
	if dev.A.Path != "/arr-stack-dev" || dev.B.Path != "/apps/arr-stack" {
		t.Errorf("dev pair paths = %q -> %q, want the per-env overrides", dev.A.Path, dev.B.Path)
	}
}

func TestFolderMappingIsIndependentOnEachSide(t *testing.T) {
	doc := strings.NewReplacer(
		"a: {instance: cloud, project: alekc-home-cluster, path: /}",
		"a: {instance: cloud, project: alekc-home-cluster, path: /arr-stack}",
		"b: {instance: selfhosted, project: home-cluster-k-wv9, path: /}",
		"b: {instance: selfhosted, project: home-cluster-k-wv9, path: /apps/arr-stack/}",
	).Replace(base)

	p := mustParse(t, doc).Pairs()[0]
	if p.A.Path != "/arr-stack" {
		t.Errorf("A.Path = %q, want /arr-stack", p.A.Path)
	}
	if p.B.Path != "/apps/arr-stack" {
		t.Errorf("B.Path = %q, want /apps/arr-stack with the trailing slash normalised away", p.B.Path)
	}
}

func TestStateScopeTracksTheResolvedFolders(t *testing.T) {
	first := mustParse(t, base).Pairs()[0].StateScope()

	moved := strings.Replace(base,
		"b: {instance: selfhosted, project: home-cluster-k-wv9, path: /}",
		"b: {instance: selfhosted, project: home-cluster-k-wv9, path: /mirror}", 1)
	second := mustParse(t, moved).Pairs()[0].StateScope()

	if first == second {
		t.Errorf("StateScope() = %q for both roots, want re-pointing a rule to start from no state", first)
	}
}

func TestOverlappingWritesRejected(t *testing.T) {
	doc := base + `  - name: arr-stack
    mode: a-to-b
    a: {instance: cloud, project: alekc-home-cluster, path: /arr-stack}
    b: {instance: selfhosted, project: home-cluster-k-wv9, path: /apps/arr-stack}
    envMap:
      prod: prod
`
	_, err := parse(t, doc)
	if err == nil {
		t.Fatal("Parse() = nil error, want the two rules to be rejected as overlapping writers")
	}
	if !strings.Contains(err.Error(), "overlapping") {
		t.Errorf("error = %v, want it to name the overlap", err)
	}
}

func TestOverlapAllowedWhenTheWiderRuleCarvesItOut(t *testing.T) {
	doc := strings.Replace(base, "    envMap:\n      prod: prod\n",
		"    exclude: [\"/apps/arr-stack/**\"]\n    envMap:\n      prod: prod\n", 1) +
		`  - name: arr-stack
    mode: a-to-b
    a: {instance: cloud, project: alekc-home-cluster, path: /arr-stack}
    b: {instance: selfhosted, project: home-cluster-k-wv9, path: /apps/arr-stack}
    envMap:
      prod: prod
`
	if _, err := parse(t, doc); err != nil {
		t.Errorf("Parse() error = %v, want the carve-out to be accepted", err)
	}
}

func TestCarveOutMustCoverTheSubtreeToo(t *testing.T) {
	// The folder itself is excluded, its children are not, and the narrow rule
	// is recursive: still an overlap.
	doc := strings.Replace(base, "    envMap:\n      prod: prod\n",
		"    exclude: [\"/apps/arr-stack\"]\n    envMap:\n      prod: prod\n", 1) +
		`  - name: arr-stack
    mode: a-to-b
    a: {instance: cloud, project: alekc-home-cluster, path: /arr-stack}
    b: {instance: selfhosted, project: home-cluster-k-wv9, path: /apps/arr-stack}
    envMap:
      prod: prod
`
	_, err := parse(t, doc)
	if err == nil {
		t.Fatal("Parse() = nil error, want a half-done carve-out to be rejected")
	}
	if !strings.Contains(err.Error(), "overlapping") {
		t.Errorf("error = %v, want it to name the overlap", err)
	}
}

func TestExcludesSubtree(t *testing.T) {
	tests := []struct {
		name      string
		exclude   []string
		path      string
		recursive bool
		want      bool
	}{
		{name: "trailing doublestar covers the folder and its subtree", exclude: []string{"/apps/arr/**"}, path: "/apps/arr", recursive: true, want: true},
		{name: "folder and subtree spelled out separately", exclude: []string{"/apps/arr", "/apps/arr/**"}, path: "/apps/arr", recursive: true, want: true},
		{name: "only the folder excluded, narrow rule recursive", exclude: []string{"/apps/arr"}, path: "/apps/arr", recursive: true, want: false},
		{name: "only the folder excluded, narrow rule not recursive", exclude: []string{"/apps/arr"}, path: "/apps/arr", recursive: false, want: true},
		{name: "single level wildcard does not cover the subtree", exclude: []string{"/apps/arr", "/apps/arr/*"}, path: "/apps/arr", recursive: true, want: false},
		{name: "nothing excluded", path: "/apps/arr", recursive: true, want: false},
		{name: "include does not count as an exclusion", exclude: nil, path: "/apps/arr", recursive: false, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Pair{Exclude: tt.exclude}
			if got := p.ExcludesSubtree(tt.path, tt.recursive); got != tt.want {
				t.Errorf("ExcludesSubtree(%q, %v) = %v, want %v", tt.path, tt.recursive, got, tt.want)
			}
		})
	}
}

func TestRelativeTo(t *testing.T) {
	tests := []struct {
		abs, root, want string
		ok              bool
	}{
		{abs: "/apps/arr", root: "/", want: "/apps/arr", ok: true},
		{abs: "/apps/arr", root: "/apps", want: "/arr", ok: true},
		{abs: "/apps", root: "/apps", want: "/", ok: true},
		{abs: "/", root: "/", want: "/", ok: true},

		// A sibling sharing a string prefix with the root. Trimming bytes
		// rather than path segments turned each of these into a plausible
		// folder inside the root: "/apps/x" under "/app" came back as "/s/x",
		// which was then matched against the rule's patterns as though real.
		{abs: "/apps/x", root: "/app"},
		{abs: "/application/x", root: "/app"},
		{abs: "/appsX", root: "/apps"},
		{abs: "/elsewhere/x", root: "/apps/arr"},
		{abs: "/", root: "/apps"},
	}

	for _, tt := range tests {
		got, ok := RelativeTo(tt.abs, tt.root)
		if ok != tt.ok {
			t.Errorf("RelativeTo(%q, %q) ok = %v, want %v (got %q)", tt.abs, tt.root, ok, tt.ok, got)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("RelativeTo(%q, %q) = %q, want %q", tt.abs, tt.root, got, tt.want)
		}
	}
}

func TestExampleConfigParses(t *testing.T) {
	cfg, err := Load("../../examples/config.yaml")
	if err != nil {
		t.Fatalf("examples/config.yaml does not validate: %v", err)
	}
	if len(cfg.Pairs()) != 4 {
		t.Errorf("Pairs() = %d, want 4 (two rules, two environments each)", len(cfg.Pairs()))
	}
}

func TestNonOverlappingWritesAccepted(t *testing.T) {
	// Same destination tree, but the second rule only ever reads from it.
	doc := base + `  - name: arr-stack
    mode: b-to-a
    a: {instance: cloud, project: other-project, path: /arr-stack}
    b: {instance: selfhosted, project: home-cluster-k-wv9, path: /apps/arr-stack}
    envMap:
      prod: prod
`
	if _, err := parse(t, doc); err != nil {
		t.Errorf("Parse() error = %v, want a read-only second rule to be allowed", err)
	}
}

func TestDifferentEnvironmentsDoNotOverlap(t *testing.T) {
	doc := strings.Replace(base, "      prod: prod\n", "      prod: prod\n      dev: dev\n", 1)
	if _, err := parse(t, doc); err != nil {
		t.Errorf("Parse() error = %v, want two environments of one rule to coexist", err)
	}
}

func TestPairWriteScopes(t *testing.T) {
	tests := []struct {
		mode Mode
		want int
	}{
		{ModeAToB, 1},
		{ModeBToA, 1},
		{ModeBidirectional, 2},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			doc := strings.Replace(base, "mode: bidirectional", "mode: "+string(tt.mode), 1)
			p := mustParse(t, doc).Pairs()[0]

			if got := len(p.WriteScopes()); got != tt.want {
				t.Fatalf("WriteScopes() = %d scopes, want %d", got, tt.want)
			}
			switch tt.mode {
			case ModeAToB:
				if !p.Writes(p.B) || p.Writes(p.A) {
					t.Errorf("a-to-b writes A=%v B=%v, want only B", p.Writes(p.A), p.Writes(p.B))
				}
			case ModeBToA:
				if !p.Writes(p.A) || p.Writes(p.B) {
					t.Errorf("b-to-a writes A=%v B=%v, want only A", p.Writes(p.A), p.Writes(p.B))
				}
			}
		})
	}
}

func TestSelects(t *testing.T) {
	tests := []struct {
		name      string
		recursive bool
		include   []string
		exclude   []string
		path      string
		want      bool
	}{
		{name: "root always selected", recursive: true, path: "/", want: true},
		{name: "subfolder when recursive", recursive: true, path: "/arr-stack", want: true},
		{name: "subfolder when not recursive", recursive: false, path: "/arr-stack", want: false},
		{name: "root when not recursive", recursive: false, path: "/", want: true},
		{name: "include matches", recursive: true, include: []string{"/arr-stack/**"}, path: "/arr-stack/deep", want: true},
		{name: "include does not match", recursive: true, include: []string{"/arr-stack/**"}, path: "/other", want: false},
		{name: "include without leading slash", recursive: true, include: []string{"arr-stack"}, path: "/arr-stack", want: true},
		{name: "exclude wins over include", recursive: true, include: []string{"**"}, exclude: []string{"/scratch/**"}, path: "/scratch/x", want: false},
		{name: "exclude misses", recursive: true, include: []string{"**"}, exclude: []string{"/scratch/**"}, path: "/keep", want: true},
		{name: "unnormalised input", recursive: true, path: "arr-stack/", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Pair{Recursive: tt.recursive, Include: tt.include, Exclude: tt.exclude}
			if got := p.Selects(tt.path); got != tt.want {
				t.Errorf("Selects(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestScopeOverlaps(t *testing.T) {
	root := Scope{Instance: "self", Project: "p", Env: "prod", Path: "/"}
	sub := Scope{Instance: "self", Project: "p", Env: "prod", Path: "/apps/arr"}
	otherEnv := Scope{Instance: "self", Project: "p", Env: "dev", Path: "/apps/arr"}
	sibling := Scope{Instance: "self", Project: "p", Env: "prod", Path: "/apps/other"}

	tests := []struct {
		name                   string
		a, b                   Scope
		aRecursive, bRecursive bool
		want                   bool
	}{
		{name: "recursive root contains subfolder", a: root, b: sub, aRecursive: true, want: true},
		{name: "non-recursive root does not", a: root, b: sub, want: false},
		{name: "subfolder inside recursive root, other direction", a: sub, b: root, bRecursive: true, want: true},
		{name: "same path always overlaps", a: sub, b: sub, want: true},
		{name: "siblings never overlap", a: sub, b: sibling, aRecursive: true, bRecursive: true, want: false},
		{name: "different environment never overlaps", a: sub, b: otherEnv, aRecursive: true, bRecursive: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Overlaps(tt.b, tt.aRecursive, tt.bRecursive); got != tt.want {
				t.Errorf("Overlaps() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStateSaltEnv(t *testing.T) {
	cfg := mustParse(t, strings.Replace(base,
		"state:\n  backend: file\n",
		"state:\n  backend: file\n  saltEnv: MIRROR_STATE_SALT\n", 1))
	if cfg.State.SaltEnv != "MIRROR_STATE_SALT" {
		t.Errorf("state.saltEnv = %q", cfg.State.SaltEnv)
	}

	// Omitted is valid: the store then generates a salt and keeps it.
	if got := mustParse(t, base).State.SaltEnv; got != "" {
		t.Errorf("state.saltEnv with nothing set = %q, want empty", got)
	}

	// The field names a variable. A salt pasted here would be a secret written
	// into a file that usually ends up committed somewhere, so the shape is
	// checked rather than accepted.
	rejects := map[string]string{
		"a pasted salt":     "hunter2-with-punctuation!",
		"spaces":            "MIRROR SALT",
		"leading digit":     "1MIRROR_SALT",
		"surrounding space": " MIRROR_SALT",
	}
	for name, value := range rejects {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(base,
				"state:\n  backend: file\n",
				"state:\n  backend: file\n  saltEnv: \""+value+"\"\n", 1)
			if _, err := parse(t, doc); err == nil {
				t.Fatalf("saltEnv %q should be rejected", value)
			}
		})
	}
}

// A rule whose two sides sit inside each other feeds itself: /mirror to
// /mirror/sub copies /mirror/x onward, one level deeper every run. Neither
// runtime guard catches it (every action is a create, and no side is ever
// empty), so config is the only place it can be stopped.
func TestSelfOverlappingRuleRejected(t *testing.T) {
	const sameSide = "b: {instance: selfhosted, project: home-cluster-k-wv9, path: /}"

	rejected := map[string]string{
		"b inside a":          "b: {instance: cloud, project: alekc-home-cluster, path: /sub}",
		"a inside b":          "b: {instance: cloud, project: alekc-home-cluster, path: /}",
		"the identical scope": "b: {instance: cloud, project: alekc-home-cluster, path: /}",
	}
	for name, replacement := range rejected {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(base, sameSide, replacement, 1)
			if name == "a inside b" {
				doc = strings.Replace(doc,
					"a: {instance: cloud, project: alekc-home-cluster, path: /}",
					"a: {instance: cloud, project: alekc-home-cluster, path: /sub}", 1)
			}

			_, err := parse(t, doc)
			if err == nil {
				t.Fatal("want an error for a rule that overlaps itself")
			}
			// The message has to say what to do about it, because the shape of
			// the failure at runtime (endless creates) does not point here.
			for _, want := range []string{"overlap", "separate folders"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to mention %q", err, want)
				}
			}
		})
	}

	// The cases that must keep working. Two sides on one instance are fine as
	// long as something separates them: a different project, a different
	// environment, or two folders neither of which contains the other.
	allowed := map[string]string{
		"sibling folders on one instance": "b: {instance: cloud, project: alekc-home-cluster, path: /other}",
		"a different project":             "b: {instance: cloud, project: another-project, path: /}",
	}
	for name, replacement := range allowed {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(base, sameSide, replacement, 1)
			doc = strings.Replace(doc,
				"a: {instance: cloud, project: alekc-home-cluster, path: /}",
				"a: {instance: cloud, project: alekc-home-cluster, path: /mirror}", 1)
			if _, err := parse(t, doc); err != nil {
				t.Errorf("Parse() = %v, want it accepted", err)
			}
		})
	}

	// Two different environments on the same instance, project and path is the
	// case a path-only check would wrongly reject: it is the ordinary shape of
	// a prod-to-staging mirror.
	doc := strings.Replace(base, sameSide,
		"b: {instance: cloud, project: alekc-home-cluster, path: /}", 1)
	doc = strings.Replace(doc, "      prod: prod\n", "      prod: staging\n", 1)
	if _, err := parse(t, doc); err != nil {
		t.Errorf("Parse() = %v, want two environments on one path accepted", err)
	}
}

func TestDaemonAndMetricsDefaults(t *testing.T) {
	cfg := mustParse(t, base)

	if got := cfg.Daemon.Interval.Duration(); got != DefaultDaemonInterval {
		t.Errorf("daemon.interval = %s, want %s", got, DefaultDaemonInterval)
	}
	if got := cfg.Metrics.Listen; got != DefaultMetricsListen {
		t.Errorf("metrics.listen = %q, want %q", got, DefaultMetricsListen)
	}
	// No default: a textfile written to a path no collector reads is worse
	// than no file, because it looks like reporting.
	if got := cfg.Metrics.Textfile; got != "" {
		t.Errorf("metrics.textfile = %q, want it unset unless asked for", got)
	}
}

func TestDaemonIntervalTakesTheShorthand(t *testing.T) {
	for _, tc := range []struct {
		doc  string
		want string
	}{
		{"daemon:\n  interval: 90s\n", "1m30s"},
		{"daemon:\n  interval: 15m\n", "15m0s"},
		{"daemon:\n  interval: 1h30m\n", "1h30m0s"},
		{"daemon:\n  interval: \"2h\"\n", "2h0m0s"},
	} {
		cfg := mustParse(t, base+tc.doc)
		if got := cfg.Daemon.Interval.String(); got != tc.want {
			t.Errorf("interval %q parsed as %s, want %s", tc.doc, got, tc.want)
		}
	}
}

// TestABareNumberIsNotADuration is the point of the custom type. yaml.v3 would
// decode a bare number into a time.Duration as nanoseconds, so `interval: 15`
// would mean fifteen nanoseconds and the daemon would reconcile in a hot loop
// against two live instances.
func TestABareNumberIsNotADuration(t *testing.T) {
	for _, doc := range []string{
		"daemon:\n  interval: 15\n",
		"daemon:\n  interval: 0\n",
		"daemon:\n  interval: 15 minutes\n",
		"daemon:\n  interval: true\n",
	} {
		if _, err := parse(t, base+doc); err == nil {
			t.Errorf("%q was accepted as a duration", strings.TrimSpace(doc))
		}
	}
}

func TestDaemonIntervalHasAFloor(t *testing.T) {
	if _, err := parse(t, base+"daemon:\n  interval: 5s\n"); err == nil {
		t.Fatal("a 5s cadence was accepted; every pass lists both sides of every rule in full")
	} else if !strings.Contains(err.Error(), "below the") {
		t.Errorf("error %q does not explain the floor", err)
	}

	// The floor itself is allowed, so the message names a value that works.
	if _, err := parse(t, base+"daemon:\n  interval: 1m\n"); err != nil {
		t.Errorf("the minimum itself was rejected: %v", err)
	}
}

func TestMetricsListenMustBeAnAddress(t *testing.T) {
	for _, addr := range []string{"9090", "not an address", "localhost"} {
		doc := base + "metrics:\n  listen: \"" + addr + "\"\n"
		if _, err := parse(t, doc); err == nil {
			t.Errorf("metrics.listen %q was accepted", addr)
		}
	}
	for _, addr := range []string{":9090", "127.0.0.1:9090", "[::1]:9090"} {
		doc := base + "metrics:\n  listen: \"" + addr + "\"\n"
		if _, err := parse(t, doc); err != nil {
			t.Errorf("metrics.listen %q was rejected: %v", addr, err)
		}
	}
}

// TestMetricsTextfileMustBeAbsoluteAndDotProm covers the two ways a textfile is
// written correctly and read by nothing: a relative path, which under a CronJob
// lands in whatever the image's WORKDIR happens to be, and a name the collector
// filters out.
func TestMetricsTextfileMustBeAbsoluteAndDotProm(t *testing.T) {
	for _, path := range []string{"metrics.prom", "./m.prom", "/var/lib/node_exporter/m.txt", "/var/lib/m"} {
		doc := base + "metrics:\n  textfile: " + path + "\n"
		if _, err := parse(t, doc); err == nil {
			t.Errorf("metrics.textfile %q was accepted", path)
		}
	}
	doc := base + "metrics:\n  textfile: /var/lib/node_exporter/textfile_collector/infisical_mirror.prom\n"
	if _, err := parse(t, doc); err != nil {
		t.Errorf("a well-formed textfile path was rejected: %v", err)
	}
}
