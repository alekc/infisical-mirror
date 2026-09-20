package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/alekc/infisical-mirror/internal/config"
	"github.com/alekc/infisical-mirror/internal/metrics"
	"github.com/alekc/infisical-mirror/internal/reconcile"
	"github.com/alekc/infisical-mirror/internal/state"
)

func runApply(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Carry out what plan would do.\n\nFlags:\n")
		fs.PrintDefaults()
	}

	configPath := fs.String("config", "", "path to the configuration file (required)")
	dryRun := fs.Bool("dry-run", false, "mark every rule as dry run, whatever the config says; writes nothing")
	force := fs.Bool("force", false, "carry out a rule whose guards fired")
	daemon := fs.Bool("daemon", false, "stay up, apply on the configured cadence, and serve /metrics")
	var rules stringList
	fs.Var(&rules, "rule", "apply only this rule; repeatable")

	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *configPath == "" {
		fmt.Fprintln(stderr, "apply: --config is required")
		return ExitUsage
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "apply: %v\n", err)
		return ExitError
	}
	if *dryRun {
		cfg.ForceDryRun()
	}

	pairs, err := cfg.RulesNamed(rules...)
	if err != nil {
		fmt.Fprintf(stderr, "apply: %v\n", err)
		return ExitUsage
	}
	if len(pairs) == 0 {
		fmt.Fprintln(stderr, "apply: the configuration has no rules to apply")
		return ExitError
	}

	writers, readers, err := buildBothWays(cfg, pairs)
	if err != nil {
		fmt.Fprintf(stderr, "apply: %v\n", err)
		return ExitError
	}

	// The exclusive lock, held for the life of the process. Two appliers on one
	// state file would each write back a document missing the other's work, and
	// a daemon plus a CronJob on the same path is the ordinary way that happens.
	// This is what makes the second one refuse rather than corrupt.
	store, err := openStore(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "apply: %v\n", err)
		return ExitError
	}
	defer store.Close()

	// The planner reads through the narrowed Reader even here. Apply needs a
	// plan before it needs a writer, and computing it through the same
	// read-only value the plan command uses is what keeps the two commands
	// deciding identically.
	planner := reconcile.NewPlanner(readers, store)
	reg := metrics.New()
	reg.SetBuildInfo(Version, Commit, BuildDate)

	if *force {
		fmt.Fprintln(stderr, "apply: --force is set, so a guard will be reported and overridden rather than stopping the rule")
	}

	onePass := func(ctx context.Context) int {
		started := time.Now()
		reg.BeginPass()

		exit := ExitOK
		for _, pair := range pairs {
			exit = worst(exit, applyOne(ctx, planner, writers, store, reg, pair, *force, stdout, stderr))
		}

		reg.EndPass("apply", started, exit != ExitError)
		return exit
	}

	return serve(ctx, cfg, "apply", *daemon, reg, onePass, stdout, stderr)
}

// applyOne plans a single rule, carries it out, and persists what landed.
func applyOne(
	ctx context.Context,
	planner *reconcile.Planner,
	clients map[string]reconcile.Writer,
	store state.Store,
	reg *metrics.Registry,
	pair config.Pair,
	force bool,
	stdout, stderr io.Writer,
) int {
	id := ruleID(pair)

	plan, err := planner.Plan(ctx, pair)
	if err != nil {
		// One unreachable instance should not stop the other rules from being
		// applied, the same way it does not stop them from being planned.
		fmt.Fprintf(stderr, "apply: %v\n", err)
		reg.ObserveRuleError(id)
		return ExitError
	}

	printPlan(stdout, plan)
	reg.ObservePlan(id, planCounts(plan))

	var opts []reconcile.ApplyOption
	if force {
		opts = append(opts, reconcile.Force())
	}

	res, err := reconcile.Apply(ctx, clients, plan, opts...)
	if err != nil {
		if errors.Is(err, reconcile.ErrBlocked) {
			// Not an error in the sense of something going wrong: the tool
			// read both sides correctly and refused. It gets its own exit code
			// because the thing to do about it is different.
			fmt.Fprintf(stderr, "apply: rule %s refused, nothing was written\n", pair.Rule)
			return ExitBlocked
		}
		fmt.Fprintf(stderr, "apply: rule %s: %v\n", pair.Rule, err)
		reg.ObserveRuleError(id)
		return ExitError
	}

	if res.DryRun {
		fmt.Fprintf(stdout, "  dry run, nothing written and no state recorded\n\n")
		return ExitOK
	}

	reg.ObserveApply(id, map[string]int{
		string(reconcile.OpCreate): res.Created,
		string(reconcile.OpUpdate): res.Updated,
		string(reconcile.OpDelete): res.Deleted,
	}, res.Failed)

	for _, e := range res.Errors {
		fmt.Fprintf(stderr, "apply: rule %s: %v\n", pair.Rule, e)
	}
	fmt.Fprintf(stdout, "  applied: %d created, %d updated, %d deleted", res.Created, res.Updated, res.Deleted)
	if res.Failed > 0 {
		fmt.Fprintf(stdout, ", %d failed and left for the next pass", res.Failed)
	}
	fmt.Fprintln(stdout)

	// Saved even when some batches failed, because the state has already been
	// reverted for exactly those and holds the successes. Skipping the save
	// would throw away the writes that did land, and the next pass would then
	// see them as a second round of drift.
	if err := store.Save(ctx, plan.Scope, res.State); err != nil {
		// This is the worst failure in the command: the instances changed and
		// the record of it did not. Saying so plainly matters more than the
		// exit code, because the recovery is a human reading the next plan
		// with this message in mind.
		fmt.Fprintf(stderr, "apply: rule %s: the writes landed but the state could not be saved, so the next pass will re-examine them: %v\n", pair.Rule, err)
		return ExitError
	}
	fmt.Fprintln(stdout)

	if len(res.Errors) > 0 {
		return ExitError
	}
	return ExitOK
}

// buildBothWays builds one client per instance and hands back two views of it:
// the writers apply carries the plan out through, and the read-only readers the
// planning half runs on. Narrowing the read side even inside apply is
// deliberate. See docs/design.md.
func buildBothWays(cfg *config.Config, pairs []config.Pair) (map[string]reconcile.Writer, map[string]reconcile.Reader, error) {
	raw, err := buildRawClients(cfg, pairs)
	if err != nil {
		return nil, nil, err
	}

	writers := make(map[string]reconcile.Writer, len(raw))
	readers := make(map[string]reconcile.Reader, len(raw))
	for name, c := range raw {
		writers[name] = c
		readers[name] = readOnly{c}
	}
	return writers, readers, nil
}
