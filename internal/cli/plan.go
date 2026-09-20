package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/alekc/infisical-mirror/internal/config"
	"github.com/alekc/infisical-mirror/internal/infisical"
	"github.com/alekc/infisical-mirror/internal/metrics"
	"github.com/alekc/infisical-mirror/internal/reconcile"
	"github.com/alekc/infisical-mirror/internal/state"
)

func runPlan(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Read both sides of every rule and print what would change.\n\nFlags:\n")
		fs.PrintDefaults()
	}

	configPath := fs.String("config", "", "path to the configuration file (required)")
	exitCode := fs.Bool("exit-code", false, "exit "+fmt.Sprint(ExitDrift)+" when any rule has pending changes")
	dryRun := fs.Bool("dry-run", false, "mark every rule as dry run, whatever the config says")
	daemon := fs.Bool("daemon", false, "stay up, plan on the configured cadence, and serve /metrics")
	var rules stringList
	fs.Var(&rules, "rule", "plan only this rule; repeatable")

	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *configPath == "" {
		fmt.Fprintln(stderr, "plan: --config is required")
		return ExitUsage
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "plan: %v\n", err)
		return ExitError
	}
	if *dryRun {
		cfg.ForceDryRun()
	}

	pairs, err := cfg.RulesNamed(rules...)
	if err != nil {
		fmt.Fprintf(stderr, "plan: %v\n", err)
		return ExitUsage
	}
	if len(pairs) == 0 {
		fmt.Fprintln(stderr, "plan: the configuration has no rules to plan")
		return ExitError
	}

	clients, err := buildClients(cfg, pairs)
	if err != nil {
		fmt.Fprintf(stderr, "plan: %v\n", err)
		return ExitError
	}
	// Read-only: a shared lock, so an operator running plan while a scheduled
	// pass holds the file is told what it says rather than refused.
	store, err := openStore(cfg, state.ReadOnly())
	if err != nil {
		fmt.Fprintf(stderr, "plan: %v\n", err)
		return ExitError
	}
	defer store.Close()

	planner := reconcile.NewPlanner(clients, store)
	reg := metrics.New()
	reg.SetBuildInfo(Version, Commit, BuildDate)

	onePass := func(ctx context.Context) int {
		started := time.Now()
		reg.BeginPass()

		exit := ExitOK
		for _, pair := range pairs {
			plan, err := planner.Plan(ctx, pair)
			if err != nil {
				// One unreachable instance should not hide what the other rules
				// have to say, so a failure is reported and the run continues.
				fmt.Fprintf(stderr, "plan: %v\n", err)
				reg.ObserveRuleError(ruleID(pair))
				exit = worst(exit, ExitError)
				continue
			}

			printPlan(stdout, plan)
			reg.ObservePlan(ruleID(pair), planCounts(plan))

			switch {
			case plan.Blocked():
				exit = worst(exit, ExitBlocked)
			case *exitCode && len(plan.Actions) > 0:
				exit = worst(exit, ExitDrift)
			}
		}

		// Drift is not a failed pass. A mirror with pending changes has done
		// its job by finding them, so alerting on staleness of the success
		// timestamp would fire on exactly the runs that worked.
		reg.EndPass("plan", started, exit != ExitError)
		return exit
	}

	return serve(ctx, cfg, "plan", *daemon, reg, onePass, stdout, stderr)
}

// worst keeps the most serious exit code seen. An error outranks a refusal,
// which outranks drift.
func worst(a, b int) int {
	rank := map[int]int{ExitOK: 0, ExitDrift: 1, ExitBlocked: 2, ExitError: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// buildClients builds one read-only client per instance the selected rules use.
func buildClients(cfg *config.Config, pairs []config.Pair) (map[string]reconcile.Reader, error) {
	raw, err := buildRawClients(cfg, pairs)
	if err != nil {
		return nil, err
	}
	clients := make(map[string]reconcile.Reader, len(raw))
	for name, c := range raw {
		clients[name] = readOnly{c}
	}
	return clients, nil
}

// buildRawClients builds one client per instance the selected rules actually
// use, so planning one rule needs no credentials for an instance it never
// touches. The result is unnarrowed: every caller narrows it before the
// reconciler sees it, once, here at the edge. See docs/design.md.
func buildRawClients(cfg *config.Config, pairs []config.Pair) (map[string]*infisical.Client, error) {
	needed := make(map[string]bool)
	for _, p := range pairs {
		needed[p.A.Instance] = true
		needed[p.B.Instance] = true
	}

	clients := make(map[string]*infisical.Client, len(needed))
	for name := range needed {
		inst, ok := cfg.Instances[name]
		if !ok {
			// Unreachable: validation resolves every reference. Checked
			// anyway, because the alternative is a nil client.
			return nil, fmt.Errorf("instance %q is not configured", name)
		}

		var idEnv, secretEnv, tokenEnv string
		if ua := inst.Auth.UniversalAuth; ua != nil {
			idEnv, secretEnv = ua.ClientIDEnv, ua.ClientSecretEnv
		}
		if t := inst.Auth.Token; t != nil {
			tokenEnv = t.TokenEnv
		}

		tokens, err := infisical.TokenSourceFromEnv(idEnv, secretEnv, tokenEnv)
		if err != nil {
			return nil, fmt.Errorf("instance %s: %w", name, err)
		}
		client, err := infisical.New(inst.URL, tokens)
		if err != nil {
			return nil, fmt.Errorf("instance %s: %w", name, err)
		}
		clients[name] = client
	}
	return clients, nil
}

// readOnly narrows a client to the two methods a plan may call. The Reader
// interface already has only those two, but an interface value holding an
// *infisical.Client hands every write method back to one type assertion. See
// docs/design.md.
type readOnly struct{ c *infisical.Client }

func (r readOnly) ResolveProjectID(ctx context.Context, slug string, environments ...string) (string, error) {
	return r.c.ResolveProjectID(ctx, slug, environments...)
}

func (r readOnly) ListSecrets(ctx context.Context, req infisical.ListRequest) ([]infisical.Secret, error) {
	return r.c.ListSecrets(ctx, req)
}

func openStore(cfg *config.Config, opts ...state.FileOption) (state.Store, error) {
	switch cfg.State.Backend {
	case config.StateBackendFile:
		if cfg.State.SaltEnv != "" {
			salt, err := state.SaltFromEnv(cfg.State.SaltEnv)
			if err != nil {
				return nil, err
			}
			opts = append(opts, state.WithSalt(salt))
		}
		return state.OpenFile(cfg.State.File.Path, opts...)
	default:
		return nil, errors.New("unsupported state backend " + string(cfg.State.Backend))
	}
}
