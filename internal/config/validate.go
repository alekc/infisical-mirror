package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Validate checks the whole document and reports every problem it finds, not
// just the first. It also canonicalises paths and fills defaults in place, so a
// Config that has been validated is the one the rest of the program uses.
func (c *Config) Validate() error {
	var errs []error

	errs = append(errs, c.validateInstances()...)
	errs = append(errs, c.validateState()...)
	errs = append(errs, c.validateDaemon()...)
	errs = append(errs, c.validateMetrics()...)
	errs = append(errs, c.validateWebhook()...)
	errs = append(errs, c.validateDefaults()...)
	errs = append(errs, c.validateRules()...)

	if err := errors.Join(errs...); err != nil {
		return err
	}

	// Only meaningful once every rule resolved cleanly.
	return errors.Join(c.validateNoOverlappingWrites()...)
}

func (c *Config) validateInstances() []error {
	if len(c.Instances) == 0 {
		return []error{errors.New("instances: at least one instance is required")}
	}

	var errs []error
	for _, name := range sortedKeys(c.Instances) {
		inst := c.Instances[name]
		where := fmt.Sprintf("instances.%s", name)

		switch u, err := url.Parse(inst.URL); {
		case inst.URL == "":
			errs = append(errs, fmt.Errorf("%s.url: required", where))
		case err != nil:
			errs = append(errs, fmt.Errorf("%s.url: %w", where, err))
		case u.Scheme != "http" && u.Scheme != "https":
			errs = append(errs, fmt.Errorf("%s.url: %q must be http or https", where, inst.URL))
		case u.Host == "":
			errs = append(errs, fmt.Errorf("%s.url: %q has no host", where, inst.URL))
		case u.Scheme == "http" && !isLoopback(u.Hostname()):
			// One character of a typo here would put the identity's token and
			// every secret value on the wire in the clear, permanently and with
			// nothing to notice it. Loopback is exempt because there is no wire.
			errs = append(errs, fmt.Errorf("%s.url: %q is plaintext http; use https, "+
				"since every request carries a bearer token and every reply carries secret values", where, inst.URL))
		}

		errs = append(errs, validateAuth(inst.Auth, where+".auth")...)
	}
	return errs
}

// validateAuth requires exactly one method. Two methods configured at once is
// rejected rather than resolved by precedence: which credential a process
// actually authenticated with should be readable off the config.
func validateAuth(a Auth, where string) []error {
	var errs []error

	switch {
	case a.UniversalAuth == nil && a.Token == nil:
		return []error{fmt.Errorf("%s: one of universalAuth or token is required", where)}
	case a.UniversalAuth != nil && a.Token != nil:
		return []error{fmt.Errorf("%s: universalAuth and token are mutually exclusive", where)}
	}

	if ua := a.UniversalAuth; ua != nil {
		if ua.ClientIDEnv == "" {
			errs = append(errs, fmt.Errorf("%s.universalAuth.clientIdEnv: required", where))
		}
		if ua.ClientSecretEnv == "" {
			errs = append(errs, fmt.Errorf("%s.universalAuth.clientSecretEnv: required", where))
		}
	}

	if t := a.Token; t != nil && t.TokenEnv == "" {
		errs = append(errs, fmt.Errorf("%s.token.tokenEnv: required", where))
	}
	return errs
}

func (c *Config) validateState() []error {
	if c.State.Backend == "" {
		c.State.Backend = StateBackendFile
	}
	if c.State.Backend != StateBackendFile {
		return []error{fmt.Errorf("state.backend: %q is not supported, only %q is", c.State.Backend, StateBackendFile)}
	}
	var errs []error
	if c.State.File.Path == "" {
		errs = append(errs, errors.New("state.file.path: required for the file backend"))
	}

	// The field names a variable; it is not the salt. Catching a value pasted
	// here is worth the check, since the mistake writes a secret into a file
	// that is usually committed somewhere.
	if salt := strings.TrimSpace(c.State.SaltEnv); salt != c.State.SaltEnv {
		errs = append(errs, errors.New("state.saltEnv: must not have surrounding whitespace"))
	} else if salt != "" && !envVarName.MatchString(salt) {
		errs = append(errs, fmt.Errorf("state.saltEnv: %q is not an environment variable name; name the variable holding the salt, do not put the salt here", salt))
	}
	return errs
}

// validateDaemon fills the cadence default and enforces the floor. It runs even
// for a one-shot run, because one config serves both modes: an interval that
// only fails once somebody passes --daemon is a config that passed validation
// and is still wrong.
func (c *Config) validateDaemon() []error {
	if c.Daemon.Interval == 0 {
		c.Daemon.Interval = Duration(DefaultDaemonInterval)
		return nil
	}
	if d := c.Daemon.Interval.Duration(); d < MinDaemonInterval {
		return []error{fmt.Errorf("daemon.interval: %s is below the %s minimum; every pass lists both sides of every rule in full",
			d, MinDaemonInterval)}
	}
	return nil
}

// validateMetrics fills the listen default and checks both fields, each of
// which is only read in one of the two modes.
func (c *Config) validateMetrics() []error {
	var errs []error

	if c.Metrics.Listen == "" {
		c.Metrics.Listen = DefaultMetricsListen
	}
	// SplitHostPort is the same parse net.Listen will do, so a bad address
	// fails at load rather than after the first pass has already run.
	if _, _, err := net.SplitHostPort(c.Metrics.Listen); err != nil {
		errs = append(errs, fmt.Errorf("metrics.listen: %q is not a host:port address such as :9090: %w", c.Metrics.Listen, err))
	}

	if c.Metrics.Textfile != "" {
		if !filepath.IsAbs(c.Metrics.Textfile) {
			// A CronJob's working directory is whatever the image's WORKDIR
			// happens to be, so a relative path lands somewhere nobody is
			// collecting from and the run still reports success.
			errs = append(errs, fmt.Errorf("metrics.textfile: %q must be an absolute path, because the working directory of a scheduled run is not something this config can see", c.Metrics.Textfile))
		} else if ext := filepath.Ext(c.Metrics.Textfile); ext != ".prom" {
			// The textfile collectors ignore anything that is not *.prom, so
			// the wrong extension is a file written correctly and read by
			// nothing.
			errs = append(errs, fmt.Errorf("metrics.textfile: %q must end in .prom, which is the only extension a textfile collector reads", c.Metrics.Textfile))
		}
	}
	return errs
}

// validateWebhook refuses a receiver that is only half configured: a listener
// no instance can sign for, or a secret with nowhere to arrive. Runs after
// validateDaemon and validateMetrics, whose defaults it compares against.
func (c *Config) validateWebhook() []error {
	var errs []error
	w := &c.Webhook

	var signed []string
	for _, name := range sortedKeys(c.Instances) {
		env := c.Instances[name].WebhookSecretEnv
		if env == "" {
			continue
		}
		signed = append(signed, name)
		if !envVarName.MatchString(env) {
			errs = append(errs, fmt.Errorf("instances.%s.webhookSecretEnv: %q is not an environment variable name; name the variable holding the secret, do not put the secret here", name, env))
		}
		if !w.Enabled() {
			errs = append(errs, fmt.Errorf("instances.%s.webhookSecretEnv: set, but webhook.listen is empty, so nothing would receive the webhook", name))
		}
	}

	if w.Debounce == 0 {
		w.Debounce = Duration(DefaultWebhookDebounce)
	}
	if d := w.Debounce.Duration(); d < MinWebhookDebounce || d > c.Daemon.Interval.Duration() {
		errs = append(errs, fmt.Errorf("webhook.debounce: %s must be between %s and daemon.interval (%s)",
			d, MinWebhookDebounce, c.Daemon.Interval))
	}

	if !w.Enabled() {
		return errs
	}
	if len(signed) == 0 {
		errs = append(errs, errors.New("webhook.listen: set, but no instance has a webhookSecretEnv, so every request would be refused"))
	}
	host, port, err := net.SplitHostPort(w.Listen)
	if err != nil {
		return append(errs, fmt.Errorf("webhook.listen: %q is not a host:port address such as :9091: %w", w.Listen, err))
	}
	if mHost, mPort, err := net.SplitHostPort(c.Metrics.Listen); err == nil && port == mPort &&
		(host == mHost || isWildcard(host) || isWildcard(mHost)) {
		errs = append(errs, fmt.Errorf("webhook.listen: %q collides with metrics.listen %q; the webhook gets its own port so /metrics need not be exposed with it",
			w.Listen, c.Metrics.Listen))
	}
	return errs
}

// isWildcard reports whether a listen host binds every interface.
func isWildcard(host string) bool {
	ip := net.ParseIP(host)
	return host == "" || (ip != nil && ip.IsUnspecified())
}

// envVarName is the portable shape of an environment variable name. A salt
// pasted in place of a name fails it on the first punctuation or space.
var envVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// isLoopback reports whether host is the local machine, the one place where
// plaintext http costs nothing because the traffic never reaches a wire.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Config) validateDefaults() []error {
	var errs []error

	if c.Defaults.Conflict == "" {
		c.Defaults.Conflict = DefaultConflict
	} else if !oneOf(c.Defaults.Conflict, conflictPolicies) {
		errs = append(errs, fmt.Errorf("defaults.conflict: %q is not one of %s", c.Defaults.Conflict, names(conflictPolicies)))
	}

	if c.Defaults.Delete == "" {
		c.Defaults.Delete = DefaultDelete
	} else if !oneOf(c.Defaults.Delete, deletePolicies) {
		errs = append(errs, fmt.Errorf("defaults.delete: %q is not one of %s", c.Defaults.Delete, names(deletePolicies)))
	}

	if c.Defaults.MaxChangeRatio == nil {
		c.Defaults.MaxChangeRatio = new(float64(DefaultMaxChangeRatio))
	} else if err := checkRatio(*c.Defaults.MaxChangeRatio); err != nil {
		errs = append(errs, fmt.Errorf("defaults.maxChangeRatio: %w", err))
	}

	if c.Defaults.Recursive == nil {
		c.Defaults.Recursive = new(DefaultRecursive)
	}

	if c.Defaults.DryRun == nil {
		c.Defaults.DryRun = new(DefaultDryRun)
	}
	return errs
}

func (c *Config) validateRules() []error {
	if len(c.Rules) == 0 {
		return []error{errors.New("rules: at least one rule is required")}
	}

	var errs []error
	seen := make(map[string]int, len(c.Rules))

	for i := range c.Rules {
		r := &c.Rules[i]
		where := fmt.Sprintf("rules[%d]", i)
		if r.Name != "" {
			where = fmt.Sprintf("rules.%s", r.Name)
		}

		switch {
		case r.Name == "":
			errs = append(errs, fmt.Errorf("%s.name: required", where))
		default:
			if first, dup := seen[r.Name]; dup {
				errs = append(errs, fmt.Errorf("%s.name: duplicate of rules[%d]", where, first))
			}
			seen[r.Name] = i
		}

		if !oneOf(r.Mode, modes) {
			errs = append(errs, fmt.Errorf("%s.mode: %q is not one of %s", where, r.Mode, names(modes)))
		}

		errs = append(errs, c.validateEndpoint(&r.A, where+".a")...)
		errs = append(errs, c.validateEndpoint(&r.B, where+".b")...)
		errs = append(errs, r.validateEnvMap(where)...)
		errs = append(errs, validatePatterns(r.Include, where+".include")...)
		errs = append(errs, validatePatterns(r.Exclude, where+".exclude")...)

		if r.Conflict == "" {
			r.Conflict = c.Defaults.Conflict
		} else if !oneOf(r.Conflict, conflictPolicies) {
			errs = append(errs, fmt.Errorf("%s.conflict: %q is not one of %s", where, r.Conflict, names(conflictPolicies)))
		}

		if r.Delete == "" {
			r.Delete = c.Defaults.Delete
		} else if !oneOf(r.Delete, deletePolicies) {
			errs = append(errs, fmt.Errorf("%s.delete: %q is not one of %s", where, r.Delete, names(deletePolicies)))
		}

		if r.MaxChangeRatio == nil {
			r.MaxChangeRatio = c.Defaults.MaxChangeRatio
		} else if err := checkRatio(*r.MaxChangeRatio); err != nil {
			errs = append(errs, fmt.Errorf("%s.maxChangeRatio: %w", where, err))
		}

		if r.Recursive == nil {
			r.Recursive = c.Defaults.Recursive
		}

		if r.DryRun == nil {
			r.DryRun = c.Defaults.DryRun
		}
	}
	return errs
}

func (c *Config) validateEndpoint(e *Endpoint, where string) []error {
	var errs []error

	switch {
	case e.Instance == "":
		errs = append(errs, fmt.Errorf("%s.instance: required", where))
	default:
		if _, ok := c.Instances[e.Instance]; !ok {
			errs = append(errs, fmt.Errorf("%s.instance: %q is not declared under instances", where, e.Instance))
		}
	}

	if e.Project == "" {
		errs = append(errs, fmt.Errorf("%s.project: required", where))
	}

	cleaned, err := normalizePath(e.Path)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s.path: %w", where, err))
	} else {
		e.Path = cleaned
	}
	return errs
}

func (r *Rule) validateEnvMap(where string) []error {
	if len(r.EnvMap) == 0 {
		return []error{fmt.Errorf("%s.envMap: at least one environment mapping is required", where)}
	}

	var errs []error
	for _, src := range sortedKeys(r.EnvMap) {
		m := r.EnvMap[src]
		at := fmt.Sprintf("%s.envMap.%s", where, src)

		if strings.TrimSpace(src) == "" {
			errs = append(errs, fmt.Errorf("%s: source environment slug is empty", at))
		}
		if strings.TrimSpace(m.Env) == "" {
			errs = append(errs, fmt.Errorf("%s.env: destination environment slug is empty", at))
		}

		// A fixed order, not a map: Validate promises to report every problem it
		// finds, and a config with both paths wrong would otherwise report them
		// in a different order run to run.
		for _, f := range []struct {
			field string
			p     *string
		}{{"aPath", &m.APath}, {"bPath", &m.BPath}} {
			field, p := f.field, f.p
			if *p == "" {
				continue
			}
			cleaned, err := normalizePath(*p)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s.%s: %w", at, field, err))
				continue
			}
			*p = cleaned
		}
		r.EnvMap[src] = m
	}
	return errs
}

// writeTarget is one folder tree one pair may write to.
type writeTarget struct {
	pair  Pair
	scope Scope
}

// validateNoOverlappingWrites rejects two rules that would write into the same
// folder tree, and any rule whose own two sides overlap. Two writers on one
// folder fight and the loser is a secret. Handing a subtree from a wide rule to
// a narrow one is allowed but must be said out loud. See docs/design.md.
func (c *Config) validateNoOverlappingWrites() []error {
	var targets []writeTarget
	errs := c.validateNoSelfOverlap()

	for _, p := range c.Pairs() {
		for _, s := range p.WriteScopes() {
			targets = append(targets, writeTarget{pair: p, scope: s})
		}
	}

	for i := 0; i < len(targets); i++ {
		for j := i + 1; j < len(targets); j++ {
			a, b := targets[i], targets[j]
			// A pair against itself is handled by validateNoSelfOverlap, which
			// asks a different question: not "do two rules collide" but "does
			// one rule's own two sides sit inside each other".
			if a.pair.StateScope() == b.pair.StateScope() {
				continue
			}
			if !a.scope.Overlaps(b.scope, a.pair.Recursive, b.pair.Recursive) {
				continue
			}
			if carvedOut(a, b) || carvedOut(b, a) {
				continue
			}
			errs = append(errs, fmt.Errorf(
				"rules %s and %s both write to overlapping folders: %s and %s. "+
					"Exclude the narrower folder and its subtree from the wider rule, or point one of them elsewhere",
				a.pair.Rule, b.pair.Rule, a.scope, b.scope))
		}
	}
	return errs
}

// validateNoSelfOverlap rejects a rule whose two sides sit on one instance,
// project and environment with one path inside the other, or on the same path.
// Such a rule feeds itself, one level deeper every run, and neither runtime
// guard catches it: every action is a create. See docs/design.md.
func (c *Config) validateNoSelfOverlap() []error {
	var errs []error
	for _, p := range c.Pairs() {
		if !p.A.Overlaps(p.B, p.Recursive, p.Recursive) {
			continue
		}
		errs = append(errs, fmt.Errorf(
			"rules.%s: the two sides overlap (%s and %s), so the rule would copy its own output back into "+
				"its source and nest one folder deeper on every run. Point the two sides at separate folders, "+
				"different projects, or different environments",
			p.Rule, p.A, p.B))
	}
	return errs
}

// carvedOut reports whether the wider target's rule excludes the narrower
// target's folder, which is how one rule hands a subtree over to another.
func carvedOut(wider, narrower writeTarget) bool {
	if wider.scope.Path == narrower.scope.Path {
		return false
	}
	if !narrower.scope.Within(wider.scope) {
		return false
	}
	rel, ok := RelativeTo(narrower.scope.Path, wider.scope.Path)
	if !ok {
		return false
	}
	return wider.pair.ExcludesSubtree(rel, narrower.pair.Recursive)
}

func validatePatterns(patterns []string, where string) []error {
	var errs []error
	for i, p := range patterns {
		if p == "" {
			errs = append(errs, fmt.Errorf("%s[%d]: pattern is empty", where, i))
			continue
		}
		if !doublestar.ValidatePattern(p) {
			errs = append(errs, fmt.Errorf("%s[%d]: %q is not a valid glob", where, i, p))
		}
	}
	return errs
}

// checkRatio bounds a blast-radius ratio. Zero is allowed and means "block on
// any destructive change at all", which is a reasonable setting for a folder
// nobody expects to churn; one means "never block on the ratio", which leaves
// only the absolute floor and the empty-side guard.
func checkRatio(v float64) error {
	if v < 0 || v > 1 {
		return fmt.Errorf("%v must be between 0 and 1", v)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
