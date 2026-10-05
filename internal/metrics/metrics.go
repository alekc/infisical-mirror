// Package metrics reports what a run did, to whichever sink the mode has: an
// endpoint for a daemon, a textfile for a one-shot run. Both render the same
// collectors. No secret value, key or path is ever a label; rule names,
// environments and instance names are the whole vocabulary. See docs/design.md.
package metrics

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/expfmt"
)

const namespace = "infisical_mirror"

// ruleLabels is the entire label vocabulary for a per-rule series. Adding to
// this list is the one change in this package that needs a second look: see the
// package comment for what a label costs.
var ruleLabels = []string{"rule", "env_a", "env_b"}

// Registry holds the collectors for one process.
type Registry struct {
	reg *prometheus.Registry

	buildInfo *prometheus.GaugeVec

	runTimestamp     *prometheus.GaugeVec
	successTimestamp *prometheus.GaugeVec
	runDuration      *prometheus.GaugeVec
	runsTotal        *prometheus.CounterVec

	ruleSecrets   *prometheus.GaugeVec
	ruleActions   *prometheus.GaugeVec
	ruleConverged *prometheus.GaugeVec
	ruleTracked   *prometheus.GaugeVec
	ruleBlocked   *prometheus.GaugeVec
	ruleFailed    *prometheus.GaugeVec
	ruleErrors    *prometheus.GaugeVec

	appliedTotal *prometheus.CounterVec

	passesStarted   *prometheus.CounterVec
	webhookRequests *prometheus.CounterVec
}

// New builds a registry. The collectors go on a private registry rather than
// the default one, so a dependency that registers something at init time cannot
// add series to this tool's output without anyone choosing it.
func New() *Registry {
	r := &Registry{reg: prometheus.NewRegistry()}

	gauge := func(name, help string, labels ...string) *prometheus.GaugeVec {
		v := prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help}, labels)
		r.reg.MustRegister(v)
		return v
	}
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		v := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help}, labels)
		r.reg.MustRegister(v)
		return v
	}

	r.buildInfo = gauge("build_info", "Always 1, labelled with the build this process came from.", "version", "commit", "date")

	r.runTimestamp = gauge("last_run_timestamp_seconds", "When the last pass finished, successful or not.", "command")
	r.successTimestamp = gauge("last_success_timestamp_seconds",
		"When the last pass that reached every rule without error finished. Alert on this going stale, not on the run timestamp: a run that fails every time still updates that one.", "command")
	r.runDuration = gauge("last_run_duration_seconds", "How long the last pass took.", "command")
	r.runsTotal = counter("runs_total", "Passes completed, by outcome.", "command", "result")

	r.ruleSecrets = gauge("rule_secrets", "Secrets in scope on one side of a rule, as last read.", append(ruleLabels, "side")...)
	r.ruleActions = gauge("rule_actions", "Actions in the rule's last plan, by operation.", append(ruleLabels, "op")...)
	r.ruleConverged = gauge("rule_converged", "Keys already identical on both sides.", ruleLabels...)
	r.ruleTracked = gauge("rule_tracked_keys", "Live keys the state holds for this rule.", ruleLabels...)
	r.ruleBlocked = gauge("rule_blocked", "1 when a guard refused the rule's last pass, 0 otherwise.", ruleLabels...)
	r.ruleFailed = gauge("rule_failed_actions", "Actions whose write did not land in the last apply.", ruleLabels...)
	r.ruleErrors = gauge("rule_errors", "1 when the rule's last pass could not be completed, 0 otherwise.", ruleLabels...)

	r.appliedTotal = counter("applied_total", "Writes that reached an instance, by operation.", append(ruleLabels, "op")...)

	r.passesStarted = counter("passes_started_total", "Daemon passes started, by what scheduled them: timer or webhook.", "trigger")
	// "source", not "instance": Prometheus sets instance on every scraped
	// series and would rename this one to exported_instance.
	r.webhookRequests = counter("webhook_requests_total",
		"Webhook requests, by the configured instance they named and what became of them.", "source", "outcome")

	return r
}

// ObservePassStarted counts a daemon pass by what scheduled it.
func (r *Registry) ObservePassStarted(trigger string) {
	r.passesStarted.WithLabelValues(trigger).Inc()
}

// ObserveWebhook counts one webhook request. Source is a configured instance
// name or empty, never the raw path a caller sent.
func (r *Registry) ObserveWebhook(source, outcome string) {
	r.webhookRequests.WithLabelValues(source, outcome).Inc()
}

// SetBuildInfo records the build this process came from.
func (r *Registry) SetBuildInfo(version, commit, date string) {
	r.buildInfo.Reset()
	r.buildInfo.WithLabelValues(version, commit, date).Set(1)
}

// BeginPass clears every per-rule gauge. Without it a removed or renamed rule
// reports its last values forever, and a stale gauge is worse than a missing
// one, it reads as a rule that is fine. Counters are not reset, since one that
// goes backwards is misread by every rate() over it.
func (r *Registry) BeginPass() {
	for _, v := range []*prometheus.GaugeVec{
		r.ruleSecrets, r.ruleActions, r.ruleConverged,
		r.ruleTracked, r.ruleBlocked, r.ruleFailed, r.ruleErrors,
	} {
		v.Reset()
	}
}

// RuleID names a rule in the label set. It carries no path and no secret key.
type RuleID struct {
	Rule string
	EnvA string
	EnvB string
}

func (id RuleID) values() []string { return []string{id.Rule, id.EnvA, id.EnvB} }

// PlanCounts is what one rule's plan says, reduced to numbers.
type PlanCounts struct {
	SecretsA  int
	SecretsB  int
	Actions   map[string]int // keyed by operation name
	Converged int
	Tracked   int
	Blocked   bool
}

// ObservePlan records one rule's plan.
func (r *Registry) ObservePlan(id RuleID, c PlanCounts) {
	v := id.values()
	r.ruleSecrets.WithLabelValues(append(v, "a")...).Set(float64(c.SecretsA))
	r.ruleSecrets.WithLabelValues(append(v, "b")...).Set(float64(c.SecretsB))
	for op, n := range c.Actions {
		r.ruleActions.WithLabelValues(append(v, op)...).Set(float64(n))
	}
	r.ruleConverged.WithLabelValues(v...).Set(float64(c.Converged))
	r.ruleTracked.WithLabelValues(v...).Set(float64(c.Tracked))
	r.ruleBlocked.WithLabelValues(v...).Set(boolValue(c.Blocked))
	r.ruleErrors.WithLabelValues(v...).Set(0)
}

// ObserveApply records what one rule's apply actually wrote, which is not the
// same thing as what its plan said: a batch that failed is in the plan and not
// in this.
func (r *Registry) ObserveApply(id RuleID, applied map[string]int, failed int) {
	v := id.values()
	for op, n := range applied {
		if n > 0 {
			r.appliedTotal.WithLabelValues(append(v, op)...).Add(float64(n))
		}
	}
	r.ruleFailed.WithLabelValues(v...).Set(float64(failed))
}

// ObserveRuleError records that a rule could not be completed.
func (r *Registry) ObserveRuleError(id RuleID) {
	r.ruleErrors.WithLabelValues(id.values()...).Set(1)
}

// EndPass records the pass itself. ok is false when any rule errored.
func (r *Registry) EndPass(command string, started time.Time, ok bool) {
	finished := time.Now()
	r.runTimestamp.WithLabelValues(command).Set(float64(finished.Unix()))
	r.runDuration.WithLabelValues(command).Set(finished.Sub(started).Seconds())

	result := "error"
	if ok {
		result = "success"
		r.successTimestamp.WithLabelValues(command).Set(float64(finished.Unix()))
	}
	r.runsTotal.WithLabelValues(command, result).Inc()
}

// Handler serves the endpoint a daemon exposes.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		// A collector that errors should say so in the response rather than
		// being dropped, because a silently shorter exposition looks exactly
		// like a rule that stopped existing.
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}

// WriteTextfile renders the registry to a Prometheus textfile. The write is
// temp-plus-rename in the destination directory: a collector reading a
// half-written file does not fail, it parses what is there and reports a
// truncated view as the whole one. See docs/design.md.
func (r *Registry) WriteTextfile(path string) error {
	families, err := r.reg.Gather()
	if err != nil {
		return fmt.Errorf("metrics: gathering: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".infisical-mirror-metrics-*.tmp")
	if err != nil {
		return fmt.Errorf("metrics: creating a temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Removing a file that was renamed away is a no-op, so this is only
		// reached on a failure path.
		_ = os.Remove(tmpName)
	}()

	enc := expfmt.NewEncoder(tmp, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range families {
		if err := enc.Encode(mf); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("metrics: encoding %s: %w", mf.GetName(), err)
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("metrics: syncing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("metrics: closing %s: %w", tmpName, err)
	}
	// World-readable on purpose: the collector runs as a different user and the
	// exposition holds counts, never keys or values. 0600 is right for every
	// other file here, wrong for this one. The label test keeps it honest.
	//nolint:gosec // G302: the textfile collector runs as a different user.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("metrics: setting the mode of %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("metrics: renaming into %s: %w", path, err)
	}
	return nil
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
