package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/alekc/infisical-mirror/internal/config"
	"github.com/alekc/infisical-mirror/internal/metrics"
	"github.com/alekc/infisical-mirror/internal/reconcile"
)

// pass is one complete sweep over every selected rule. It returns the exit code
// that sweep would produce if the process were exiting now.
type pass func(ctx context.Context) int

// serve runs a command in whichever of the two modes was asked for. They differ
// only in lifetime and in where the numbers go; the pass itself is the same
// function in both, so a daemon cannot drift into doing something a one-shot
// run would not. See docs/design.md.
func serve(ctx context.Context, cfg *config.Config, command string, daemon bool, reg *metrics.Registry, run pass, stdout, stderr io.Writer) int {
	if !daemon {
		exit := run(ctx)
		// Written after the pass, not before: the file is the record of what
		// happened, and a CronJob's container is usually gone moments later.
		if path := cfg.Metrics.Textfile; path != "" {
			if err := reg.WriteTextfile(path); err != nil {
				// The work is already done and reported. Failing the run over
				// its own bookkeeping would turn a reporting problem into an
				// apparent sync failure, which is the more expensive lie.
				fmt.Fprintf(stderr, "%s: %v\n", command, err)
			}
		}
		return exit
	}

	interval := cfg.Daemon.Interval.Duration()
	srv := &http.Server{
		Addr:              cfg.Metrics.Listen,
		Handler:           metricsMux(reg),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Buffered so the goroutine can exit even if nothing ever reads this, which
	// is the normal case: a listener that comes up successfully never sends.
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()
	fmt.Fprintf(stdout, "%s: serving metrics on %s, reconciling every %s\n", command, cfg.Metrics.Listen, interval)

	exit := ExitOK
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			// The pass in flight, if any, has already returned: this select is
			// only reached between passes.
			shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdown)
			fmt.Fprintf(stdout, "%s: stopping\n", command)
			return exit
		case err := <-serveErr:
			if err != nil {
				// A metrics endpoint that never came up is the whole reason to
				// be a daemon rather than a CronJob, so this is fatal rather
				// than something to carry on without.
				fmt.Fprintf(stderr, "%s: metrics endpoint: %v\n", command, err)
				return ExitError
			}
		case <-timer.C:
			// Whatever the pass returns is reported and recorded, but it does
			// not stop the loop: drift is the ordinary state of a mirror
			// between passes, and an instance that is briefly unreachable is
			// not a reason to take the metrics endpoint down with it.
			exit = run(ctx)
			// Measured from the end of the pass rather than on a fixed tick, so
			// a pass that runs longer than the interval delays the next one
			// instead of queueing a second one behind it.
			timer.Reset(interval)
		}
	}
}

// metricsMux is the daemon's HTTP surface: the endpoint and a liveness probe,
// and deliberately nothing else. This process holds credentials for two
// Infisical instances and the state that says which keys exist where; every
// route added here is reachable by anything that can reach the port.
func metricsMux(reg *metrics.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", reg.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// ruleID builds the label set for one pair. It carries the rule name and the
// two environments, and nothing else: see the metrics package comment for why a
// folder path or a secret key must never become a label.
func ruleID(pair config.Pair) metrics.RuleID {
	return metrics.RuleID{Rule: pair.Rule, EnvA: pair.A.Env, EnvB: pair.B.Env}
}

// planCounts reduces a plan to the numbers the metrics report.
func planCounts(p *reconcile.Plan) metrics.PlanCounts {
	actions := map[string]int{}
	// Every operation is seeded, so that an op dropping to zero is a zero
	// rather than a series that vanishes. A gauge that disappears and one that
	// reads zero look the same on a graph and mean opposite things.
	for _, op := range []reconcile.Op{
		reconcile.OpCreate, reconcile.OpUpdate, reconcile.OpDelete,
		reconcile.OpConflict, reconcile.OpDeleteObserved,
	} {
		actions[string(op)] = 0
	}
	for _, a := range p.Actions {
		actions[string(a.Op)]++
	}

	return metrics.PlanCounts{
		SecretsA:  p.SecretsA,
		SecretsB:  p.SecretsB,
		Actions:   actions,
		Converged: p.Converged,
		Tracked:   p.Tracked,
		Blocked:   p.Blocked(),
	}
}
