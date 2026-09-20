// Package cli is the command line surface. It is a package rather than a main
// so that a command can be run end to end in a test, against a temporary
// config, and its output asserted on.
package cli

import (
	"context"
	"fmt"
	"io"
	"runtime"
)

// Exit codes. They are part of the interface: a CronJob running `plan
// --exit-code` alerts on the difference between drift and refusal.
const (
	// ExitOK means there is nothing to do.
	ExitOK = 0
	// ExitError means the run did not complete.
	ExitError = 1
	// ExitDrift means a rule has pending changes. Only returned when
	// --exit-code asks for it.
	ExitDrift = 2
	// ExitBlocked means a guard fired: the sides could be read, but applying
	// what they say would not be safe. Always returned, with or without
	// --exit-code, because it is a condition to act on rather than a report.
	ExitBlocked = 3
	// ExitUsage means the command line itself was wrong.
	ExitUsage = 64
)

// Build metadata, set at link time with -ldflags -X. The defaults are what a
// `go build` with no flags produces, and they are deliberately obvious: a
// binary reporting "dev" is one nobody can trace back to a commit, which is
// worth knowing when it is the thing writing to production secrets.
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

// versionLine is what both `version` and `--version` print. One function, so
// the two can never drift into reporting different things.
func versionLine() string {
	return fmt.Sprintf("infisical-mirror %s (commit %s, built %s, %s)",
		Version, Commit, BuildDate, runtime.Version())
}

const usage = `infisical-mirror keeps two Infisical instances in step.

Usage:
  infisical-mirror plan   --config FILE [flags]
  infisical-mirror apply  --config FILE [flags]
  infisical-mirror state  show|prune --config FILE [flags]
  infisical-mirror version

Commands:
  plan      read both sides of every rule and print what would change
  apply     carry out what plan would do
  state     inspect or prune the sync state
  version   print the version

Both plan and apply take --daemon, which keeps the process up, reconciles on
the cadence in the config's daemon.interval, and serves /metrics. Without it a
run happens once and exits, which is the CronJob shape, and metrics go to
metrics.textfile if the config names one.

Run a command with --help for its flags.
`

// Run dispatches one command and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return ExitUsage
	}

	switch args[0] {
	case "plan":
		return runPlan(ctx, args[1:], stdout, stderr)
	case "apply":
		return runApply(ctx, args[1:], stdout, stderr)
	case "state":
		return runState(ctx, args[1:], stdout, stderr)
	case "version", "--version", "-version", "-v":
		fmt.Fprintln(stdout, versionLine())
		return ExitOK
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		fmt.Fprint(stderr, usage)
		return ExitUsage
	}
}

// stringList collects a flag that may be repeated.
type stringList []string

func (s *stringList) String() string { return fmt.Sprint(*s) }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
