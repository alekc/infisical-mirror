package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/alekc/infisical-mirror/internal/config"
	"github.com/alekc/infisical-mirror/internal/metrics"
	"github.com/alekc/infisical-mirror/internal/reconcile"
	"github.com/alekc/infisical-mirror/internal/webhook"
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
	servers := []namedServer{{"metrics endpoint", &http.Server{
		Addr:              cfg.Metrics.Listen,
		Handler:           metricsMux(reg),
		ReadHeaderTimeout: 10 * time.Second,
	}}}

	// Nil unless the webhook is on, and a nil channel never fires in a select,
	// so without a webhook the loop below is the timer alone.
	var trigger chan struct{}
	if cfg.Webhook.Enabled() {
		secrets, err := webhookSecrets(cfg)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", command, err)
			return ExitError
		}
		// One slot, filled without blocking: any number of events between two
		// passes collapse into the one pending trigger.
		trigger = make(chan struct{}, 1)
		h := &webhook.Handler{
			Secrets: secrets,
			Trigger: func() {
				select {
				case trigger <- struct{}{}:
				default:
				}
			},
			Observe: reg.ObserveWebhook,
		}
		servers = append(servers, namedServer{"webhook receiver", &http.Server{
			Addr:              cfg.Webhook.Listen,
			Handler:           h.Mux(),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       time.Minute,
			MaxHeaderBytes:    16 << 10,
		}})
	}

	// Buffered so a goroutine can exit even if nothing ever reads this, which
	// is the normal case: a listener that comes up successfully never sends.
	serveErr := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- fmt.Errorf("%s: %w", s.name, err)
			}
		}()
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for _, s := range servers {
			_ = s.srv.Shutdown(shutdown)
		}
	}()

	if trigger != nil {
		fmt.Fprintf(stdout, "%s: serving metrics on %s, webhooks on %s, reconciling every %s\n",
			command, cfg.Metrics.Listen, cfg.Webhook.Listen, interval)
	} else {
		fmt.Fprintf(stdout, "%s: serving metrics on %s, reconciling every %s\n", command, cfg.Metrics.Listen, interval)
	}

	l := loop{
		interval: interval,
		debounce: cfg.Webhook.Debounce.Duration(),
		trigger:  trigger,
		serveErr: serveErr,
		run:      run,
		started:  reg.ObservePassStarted,
	}
	exit, err := l.until(ctx)
	if err != nil {
		// A listener that never came up is the whole reason to be a daemon
		// rather than a CronJob, so this is fatal rather than carried on past.
		fmt.Fprintf(stderr, "%s: %v\n", command, err)
		return ExitError
	}
	fmt.Fprintf(stdout, "%s: stopping\n", command)
	return exit
}

type namedServer struct {
	name string
	srv  *http.Server
}

// loop schedules a daemon's passes. Passes run inline in its one goroutine,
// so two can never overlap, and a trigger that lands mid-pass waits in its
// channel and yields exactly one follow-up. See docs/design.md.
type loop struct {
	interval time.Duration
	debounce time.Duration
	trigger  <-chan struct{}
	serveErr <-chan error
	run      pass
	started  func(trigger string)
}

// until runs passes until ctx is done, returning the last pass's exit code,
// or until a listener fails, returning its error.
func (l *loop) until(ctx context.Context) (int, error) {
	exit := ExitOK
	timer := time.NewTimer(0)
	defer timer.Stop()
	// next is when the timer fires, which time.Timer does not expose.
	next := time.Now()
	byWebhook := false

	for {
		select {
		case <-ctx.Done():
			// The pass in flight, if any, has already returned: this select is
			// only reached between passes.
			return exit, nil
		case err := <-l.serveErr:
			return ExitError, err
		case <-l.trigger:
			// Only ever brings the next pass forward. A later event never
			// pushes a scheduled one back, so a steady stream cannot starve it.
			if due := time.Now().Add(l.debounce); due.Before(next) {
				timer.Reset(l.debounce)
				next, byWebhook = due, true
			}
		case <-timer.C:
			if l.started != nil {
				trigger := "timer"
				if byWebhook {
					trigger = "webhook"
				}
				l.started(trigger)
			}
			// Whatever the pass returns is reported and recorded, but it does
			// not stop the loop: drift is the ordinary state of a mirror
			// between passes, and an instance that is briefly unreachable is
			// not a reason to take the metrics endpoint down with it.
			exit = l.run(ctx)
			// Measured from the end of the pass, so a pass that runs longer
			// than the interval delays the next one instead of queueing one.
			timer.Reset(l.interval)
			next, byWebhook = time.Now().Add(l.interval), false
		}
	}
}

// webhookSecrets reads each instance's webhook secret from the environment.
// An empty one is an error: it would refuse every request from that side.
func webhookSecrets(cfg *config.Config) (map[string][]byte, error) {
	secrets := make(map[string][]byte)
	for name, inst := range cfg.Instances {
		if inst.WebhookSecretEnv == "" {
			continue
		}
		v := strings.TrimSpace(os.Getenv(inst.WebhookSecretEnv))
		if v == "" {
			return nil, fmt.Errorf("instances.%s.webhookSecretEnv: %s is unset or empty", name, inst.WebhookSecretEnv)
		}
		secrets[name] = []byte(v)
	}
	return secrets, nil
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
