// Package config loads and validates the infisical-mirror YAML configuration
// and resolves it into the concrete (env, folder) pairs the reconciler uses.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Mode is the direction a rule syncs in.
type Mode string

const (
	ModeAToB          Mode = "a-to-b"
	ModeBToA          Mode = "b-to-a"
	ModeBidirectional Mode = "bidirectional"
)

// ConflictPolicy decides what happens when both sides changed since the last
// successful sync.
type ConflictPolicy string

const (
	ConflictAWins      ConflictPolicy = "a-wins"
	ConflictBWins      ConflictPolicy = "b-wins"
	ConflictNewestWins ConflictPolicy = "newest-wins"
	ConflictFail       ConflictPolicy = "fail"
)

// DeletePolicy decides what happens when a key that both sides used to have
// disappears from one of them.
type DeletePolicy string

const (
	// DeleteIgnore reports the deletion and leaves the surviving copy alone.
	DeleteIgnore DeletePolicy = "ignore"
	// DeletePropagate deletes the surviving copy as well.
	DeletePropagate DeletePolicy = "propagate"
)

// StateBackend names a Store implementation.
type StateBackend string

const StateBackendFile StateBackend = "file"

// Defaults for every value a rule may leave unset.
const (
	DefaultConflict       = ConflictNewestWins
	DefaultDelete         = DeleteIgnore
	DefaultMaxChangeRatio = 0.25
	DefaultRecursive      = true
	DefaultDryRun         = false

	// DefaultDaemonInterval is the cadence a daemon uses when the config does
	// not name one.
	DefaultDaemonInterval = 15 * time.Minute
	// MinDaemonInterval is the floor under which a cadence is refused. Every
	// pass is a full recursive listing of both sides of every rule, so a short
	// interval is a sustained load on two live instances for no extra
	// freshness, and a typo is the usual way one gets set.
	MinDaemonInterval = time.Minute
	// DefaultMetricsListen is where a daemon's /metrics endpoint binds when the
	// config does not say. It listens on all interfaces because the only
	// deployment target is a container whose network namespace is the boundary.
	DefaultMetricsListen = ":9090"
)

// Config is the whole YAML document.
type Config struct {
	Instances map[string]Instance `yaml:"instances"`
	State     State               `yaml:"state"`
	Daemon    Daemon              `yaml:"daemon"`
	Metrics   Metrics             `yaml:"metrics"`
	Defaults  Defaults            `yaml:"defaults"`
	Rules     []Rule              `yaml:"rules"`
}

// Duration is a time.Duration that decodes from the YAML shorthand ("15m",
// "1h30m") rather than from a bare number. yaml.v3 has no time.Duration support
// and would read `interval: 15` as fifteen nanoseconds, putting the daemon in a
// hot loop against two live instances. The shorthand is the only form accepted.
type Duration time.Duration

// UnmarshalYAML accepts only the string shorthand. The tag check is what makes
// a bare number an error rather than a misinterpretation: yaml.v3 decodes the
// scalar 0 into a string and ParseDuration accepts "0", so `interval: 0` would
// otherwise parse as a zero duration and silently become the default.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Tag != "!!str" {
		return fmt.Errorf("line %d: a duration needs a unit, such as 30s, 15m or 1h, not the bare %s value %q",
			value.Line, strings.TrimPrefix(value.Tag, "!!"), value.Value)
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value.Value))
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration; use a unit such as 30s, 15m or 1h", value.Line, value.Value)
	}
	if parsed <= 0 {
		return fmt.Errorf("line %d: %q is not a positive duration", value.Line, value.Value)
	}
	*d = Duration(parsed)
	return nil
}

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// Daemon configures the long-running mode, in which the process reconciles on
// a cadence and stays up between passes so its metrics endpoint can be scraped.
type Daemon struct {
	// Interval is the cadence between passes. It is measured from the end of
	// one pass to the start of the next, not on a fixed wall-clock tick, so a
	// pass that runs long delays the next one rather than overlapping it.
	Interval Duration `yaml:"interval"`
}

// Metrics configures how a run reports itself, which differs by mode because
// the two have opposite lifetimes. A daemon is alive when Prometheus arrives,
// so it serves an endpoint; a one-shot run has usually exited before any
// scrape, so all that survives it is a file a node collector picks up later.
type Metrics struct {
	// Listen is the address the daemon's /metrics endpoint binds to. Ignored
	// by a one-shot run, which has no endpoint to serve.
	Listen string `yaml:"listen"`
	// Textfile is where a one-shot run writes the Prometheus exposition a node
	// exporter or Alloy collector scrapes. Empty, the default, reports nothing:
	// a path outside the collector's directory produces a file nobody reads,
	// which is worse than no file because it looks like reporting.
	Textfile string `yaml:"textfile"`
}

// Instance is one Infisical deployment, cloud or self-hosted.
type Instance struct {
	URL  string `yaml:"url"`
	Auth Auth   `yaml:"auth"`
}

// Auth holds exactly one authentication method. Both name environment
// variables: no credential is ever read from the config file itself, so the
// file stays safe to commit and to print.
type Auth struct {
	UniversalAuth *UniversalAuth `yaml:"universalAuth"`
	Token         *TokenAuth     `yaml:"token"`
}

// UniversalAuth names the environment variables carrying a machine identity's
// client ID and secret, which the process exchanges for an access token.
type UniversalAuth struct {
	ClientIDEnv     string `yaml:"clientIdEnv"`
	ClientSecretEnv string `yaml:"clientSecretEnv"`
}

// TokenAuth names the environment variable carrying an access or service token
// directly, for when something else already did the exchange.
type TokenAuth struct {
	TokenEnv string `yaml:"tokenEnv"`
}

// State selects and configures the sync-state store.
type State struct {
	Backend StateBackend `yaml:"backend"`
	// SaltEnv names the environment variable holding the hashing salt. Left
	// empty, the store generates one and keeps it in the state file, which is
	// simpler but self-contained: whoever holds the file can test a guessed
	// value against a hash. Naming a variable splits the two. See docs/design.md.
	SaltEnv string    `yaml:"saltEnv"`
	File    FileState `yaml:"file"`
}

// FileState configures the local-file Store.
type FileState struct {
	Path string `yaml:"path"`
}

// Defaults are applied to any rule that does not set the same field.
type Defaults struct {
	Conflict       ConflictPolicy `yaml:"conflict"`
	Delete         DeletePolicy   `yaml:"delete"`
	MaxChangeRatio *float64       `yaml:"maxChangeRatio"`
	Recursive      *bool          `yaml:"recursive"`
	DryRun         *bool          `yaml:"dryRun"`
}

// Rule pairs two folder trees and says how they are kept in step.
type Rule struct {
	Name           string                `yaml:"name"`
	Mode           Mode                  `yaml:"mode"`
	A              Endpoint              `yaml:"a"`
	B              Endpoint              `yaml:"b"`
	Recursive      *bool                 `yaml:"recursive"`
	EnvMap         map[string]EnvMapping `yaml:"envMap"`
	Include        []string              `yaml:"include"`
	Exclude        []string              `yaml:"exclude"`
	Conflict       ConflictPolicy        `yaml:"conflict"`
	Delete         DeletePolicy          `yaml:"delete"`
	MaxChangeRatio *float64              `yaml:"maxChangeRatio"`
	DryRun         *bool                 `yaml:"dryRun"`
}

// Endpoint is one side of a rule, before an environment is applied.
type Endpoint struct {
	Instance string `yaml:"instance"`
	// Project is the project slug, the identifier in the Infisical URL, not the
	// project id. The API wants the id, so it is looked up once per run; naming
	// the slug keeps the config readable and keeps a copy-pasted id from
	// silently addressing a different project on the other instance.
	Project string `yaml:"project"`
	Path    string `yaml:"path"`
}

// EnvMapping maps one environment on side A onto one on side B, optionally
// overriding either root folder for that environment alone.
//
// It accepts a bare string (`prod: prod`) as shorthand for {env: prod}.
type EnvMapping struct {
	Env   string `yaml:"env"`
	APath string `yaml:"aPath"`
	BPath string `yaml:"bPath"`
}

var envMappingFields = map[string]bool{"env": true, "aPath": true, "bPath": true}

// UnmarshalYAML accepts both the scalar shorthand and the mapping form. The
// mapping form checks its own keys because yaml.Node.Decode does not honour the
// decoder's KnownFields setting, so an unknown key here would be dropped in
// silence rather than rejected like every other unknown key in the document.
func (m *EnvMapping) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var env string
		if err := value.Decode(&env); err != nil {
			return err
		}
		m.Env = env
		return nil
	case yaml.MappingNode:
		for i := 0; i < len(value.Content); i += 2 {
			key := value.Content[i].Value
			if !envMappingFields[key] {
				return fmt.Errorf("line %d: unknown field %q in env mapping", value.Content[i].Line, key)
			}
		}
		type plain EnvMapping
		var p plain
		if err := value.Decode(&p); err != nil {
			return err
		}
		*m = EnvMapping(p)
		return nil
	default:
		return fmt.Errorf("line %d: env mapping must be an environment slug or a mapping", value.Line)
	}
}

// Load reads, parses and validates a config file.
func Load(filename string) (*Config, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cfg, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	return cfg, nil
}

// Parse reads and validates a config document. Unknown fields are rejected: a
// misspelled key that silently keeps its default is the failure mode this whole
// file exists to prevent.
func Parse(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("config is empty")
		}
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// conflictPolicies and deletePolicies exist so validation and error messages
// share one list.
var (
	conflictPolicies = []ConflictPolicy{ConflictAWins, ConflictBWins, ConflictNewestWins, ConflictFail}
	deletePolicies   = []DeletePolicy{DeleteIgnore, DeletePropagate}
	modes            = []Mode{ModeAToB, ModeBToA, ModeBidirectional}
)

func oneOf[T ~string](v T, allowed []T) bool {
	return slices.Contains(allowed, v)
}

func names[T ~string](allowed []T) string {
	out := make([]string, len(allowed))
	for i, a := range allowed {
		out[i] = string(a)
	}
	return strings.Join(out, ", ")
}

// normalizePath canonicalises a folder path to a leading slash, no trailing
// slash, no relative segments. Infisical paths are absolute within an
// environment, so anything else is a config error rather than something to
// guess at.
func normalizePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path %q must start with /", p)
	}
	// Checked before Clean, not after: path.Clean("/../x") is "/x", so a
	// cleaned path never admits to having escaped anything.
	for segment := range strings.SplitSeq(p, "/") {
		if segment == ".." {
			return "", fmt.Errorf("path %q escapes the root", p)
		}
	}
	return path.Clean(p), nil
}
