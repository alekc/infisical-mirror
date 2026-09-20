package config

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Scope is one concrete folder tree on one instance: everything needed to list
// or write secrets, with the environment already applied.
type Scope struct {
	Instance string
	Project  string
	Env      string
	Path     string
}

func (s Scope) String() string {
	return fmt.Sprintf("%s:%s:%s:%s", s.Instance, s.Project, s.Env, s.Path)
}

// Within reports whether s sits inside root, or is root itself.
func (s Scope) Within(root Scope) bool {
	if s.Instance != root.Instance || s.Project != root.Project || s.Env != root.Env {
		return false
	}
	return pathWithin(s.Path, root.Path)
}

// Overlaps reports whether two write targets can touch the same folder, given
// whether each one descends into subfolders.
func (s Scope) Overlaps(other Scope, recursive, otherRecursive bool) bool {
	if s.Instance != other.Instance || s.Project != other.Project || s.Env != other.Env {
		return false
	}
	switch {
	case s.Path == other.Path:
		return true
	case recursive && pathWithin(other.Path, s.Path):
		return true
	case otherRecursive && pathWithin(s.Path, other.Path):
		return true
	default:
		return false
	}
}

// Pair is one rule resolved down to a single environment mapping: the unit the
// reconciler actually works on, and the unit sync state is scoped to.
type Pair struct {
	Rule           string
	Mode           Mode
	A              Scope
	B              Scope
	Recursive      bool
	Include        []string
	Exclude        []string
	Conflict       ConflictPolicy
	Delete         DeletePolicy
	MaxChangeRatio float64
	DryRun         bool
}

// Pairs expands every rule across its environment mappings, in a stable order.
// Validate must have run first: it is what fills the defaults this reads.
func (c *Config) Pairs() []Pair {
	var pairs []Pair

	for _, r := range c.Rules {
		for _, src := range sortedKeys(r.EnvMap) {
			m := r.EnvMap[src]

			aPath, bPath := r.A.Path, r.B.Path
			if m.APath != "" {
				aPath = m.APath
			}
			if m.BPath != "" {
				bPath = m.BPath
			}

			pairs = append(pairs, Pair{
				Rule:           r.Name,
				Mode:           r.Mode,
				A:              Scope{Instance: r.A.Instance, Project: r.A.Project, Env: src, Path: aPath},
				B:              Scope{Instance: r.B.Instance, Project: r.B.Project, Env: m.Env, Path: bPath},
				Recursive:      derefOr(r.Recursive, DefaultRecursive),
				Include:        r.Include,
				Exclude:        r.Exclude,
				Conflict:       orDefault(r.Conflict, c.Defaults.Conflict),
				Delete:         orDefault(r.Delete, c.Defaults.Delete),
				MaxChangeRatio: derefOr(r.MaxChangeRatio, DefaultMaxChangeRatio),
				DryRun:         derefOr(r.DryRun, DefaultDryRun),
			})
		}
	}

	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].StateScope() < pairs[j].StateScope()
	})
	return pairs
}

// ForceDryRun puts every rule into dry run, whatever the file said. This is how
// a --dry-run flag works: the override only ever goes one way, so a flag can
// make a run safer and never the reverse.
func (c *Config) ForceDryRun() {
	for i := range c.Rules {
		c.Rules[i].DryRun = new(true)
	}
	c.Defaults.DryRun = new(true)
}

// RulesNamed returns the pairs belonging to the named rules, or all of them
// when no name is given. Selecting a name that no rule has is an error rather
// than an empty run, which would otherwise look like a clean sync.
func (c *Config) RulesNamed(names ...string) ([]Pair, error) {
	if len(names) == 0 {
		return c.Pairs(), nil
	}

	known := make(map[string]bool, len(c.Rules))
	for _, r := range c.Rules {
		known[r.Name] = true
	}
	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		if !known[n] {
			return nil, fmt.Errorf("no rule named %q", n)
		}
		wanted[n] = true
	}

	var out []Pair
	for _, p := range c.Pairs() {
		if wanted[p.Rule] {
			out = append(out, p)
		}
	}
	return out, nil
}

// StateScope is the key this pair's sync state is stored under. It carries the
// resolved scopes, not just the rule name, so re-pointing a rule at different
// folders starts from no state instead of inheriting hashes that describe a
// different pair of folders.
func (p Pair) StateScope() string {
	return fmt.Sprintf("%s|%s|%s", p.Rule, p.A, p.B)
}

// WriteScopes lists the sides this pair may write to.
func (p Pair) WriteScopes() []Scope {
	switch p.Mode {
	case ModeAToB:
		return []Scope{p.B}
	case ModeBToA:
		return []Scope{p.A}
	default:
		return []Scope{p.A, p.B}
	}
}

// Writes reports whether this pair may write to the given side.
func (p Pair) Writes(s Scope) bool {
	return slices.Contains(p.WriteScopes(), s)
}

// Selects reports whether a folder path relative to the pair's roots is in
// scope. relPath is "/" for the root folder itself.
//
// An empty include list means everything. Exclude always wins.
func (p Pair) Selects(relPath string) bool {
	relPath = path.Clean("/" + strings.TrimPrefix(relPath, "/"))

	if !p.Recursive && relPath != "/" {
		return false
	}

	included := len(p.Include) == 0
	for _, pattern := range p.Include {
		if matchPattern(pattern, relPath) {
			included = true
			break
		}
	}
	if !included {
		return false
	}

	for _, pattern := range p.Exclude {
		if matchPattern(pattern, relPath) {
			return false
		}
	}
	return true
}

// subtreeProbe is a folder path no Infisical folder can have, used to ask a
// glob whether it covers a whole subtree rather than just its top. Two
// segments, so a single-level pattern such as `/apps/*` does not match it and
// is correctly reported as not covering the subtree.
const subtreeProbe = "__subtree_probe__/__subtree_probe__"

// ExcludesSubtree reports whether this pair's exclude patterns keep it out of
// relPath entirely, and out of everything beneath it when recursive is true.
// Only excludes count: a narrow include list keeps a pair out too, but relying
// on that to separate two writers is fragile. See docs/design.md.
func (p Pair) ExcludesSubtree(relPath string, recursive bool) bool {
	if !p.excluded(relPath) {
		return false
	}
	if !recursive {
		return true
	}
	return p.excluded(path.Join(relPath, subtreeProbe))
}

func (p Pair) excluded(relPath string) bool {
	relPath = path.Clean("/" + strings.TrimPrefix(relPath, "/"))
	for _, pattern := range p.Exclude {
		if matchPattern(pattern, relPath) {
			return true
		}
	}
	return false
}

// RelativeTo expresses an absolute folder path as a path relative to root, with
// a leading slash. It reports false when absPath is not inside root: trimming a
// string prefix is not trimming a path prefix, and "/apps/x" trimmed by "/app"
// silently yields the plausible "/s/x". See docs/design.md.
func RelativeTo(absPath, root string) (string, bool) {
	if !pathWithin(absPath, root) {
		return "", false
	}
	if root == "/" {
		return path.Clean("/" + strings.TrimPrefix(absPath, "/")), true
	}
	return path.Clean("/" + strings.TrimPrefix(strings.TrimPrefix(absPath, root), "/")), true
}

// JoinRelative is the inverse of RelativeTo: it turns a rule-relative folder
// path back into an absolute path on one side's instance. The reconciler works
// in relative paths, the only way two differently rooted trees compare at all;
// the API works in absolute ones. See docs/design.md.
func JoinRelative(root, relPath string) string {
	root = path.Clean("/" + strings.TrimPrefix(root, "/"))
	rel := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(relPath, "/")), "/")
	if rel == "" || rel == "." {
		return root
	}
	if root == "/" {
		return "/" + rel
	}
	return root + "/" + rel
}

// matchPattern matches a glob against a relative folder path. Patterns are
// written with or without a leading slash and mean the same thing either way,
// because the paths they are matched against always have one.
func matchPattern(pattern, relPath string) bool {
	if !strings.HasPrefix(pattern, "/") {
		pattern = "/" + pattern
	}
	ok, err := doublestar.Match(pattern, relPath)
	return err == nil && ok
}

// pathWithin reports whether child is root or sits underneath it.
func pathWithin(child, root string) bool {
	if root == "/" {
		return true
	}
	return child == root || strings.HasPrefix(child, root+"/")
}

func orDefault[T ~string](v, fallback T) T {
	if v == "" {
		return fallback
	}
	return v
}

func derefOr[T any](v *T, fallback T) T {
	if v == nil {
		return fallback
	}
	return *v
}
