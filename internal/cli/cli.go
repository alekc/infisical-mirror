// Package cli is the command line surface. It is a package rather than a main
// so that a command can be run end to end in a test, against a temporary
// config, and its output asserted on.
package cli

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
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

// Build metadata, set at link time with -ldflags -X, and otherwise recovered
// from the embedded build info by init below. The defaults survive only when
// neither route has anything: a binary reporting "dev" is one nobody can trace
// back to a commit, which matters when it is what writes to production secrets.
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

func init() {
	bi, ok := debug.ReadBuildInfo()
	Version, Commit, BuildDate = resolveBuild(Version, Commit, BuildDate, bi, ok)
}

// resolveBuild fills in whichever of the three are still at their default from
// the embedded build info, taking them as arguments so the precedence is
// testable. The two sources are complementary: a proxy install carries
// Main.Version and no vcs.* at all, a checkout build carries vcs.* and no tag.
func resolveBuild(version, commit, date string, bi *debug.BuildInfo, ok bool) (string, string, string) {
	if !ok || bi == nil {
		return version, commit, date
	}

	// Trim the "v": goreleaser's .Version has no prefix, and the two routes
	// reporting 0.1.1 and v0.1.1 for one build would be a needless difference.
	if version == "dev" && isReleaseVersion(bi.Main.Version) {
		version = strings.TrimPrefix(bi.Main.Version, "v")
	}

	var modified bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if commit == "none" && s.Value != "" {
				commit = shortCommit(s.Value)
			}
		case "vcs.time":
			if date == "unknown" && s.Value != "" {
				date = s.Value
			}
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}

	// A binary built from a dirty tree is not the commit it names, and saying
	// so is the whole point of reporting a commit at all. Deliberately applied
	// to a linked commit too: `make build` on a modified tree sets one through
	// ldflags, and its VERSION already carries git describe --dirty.
	if modified && commit != "none" && !strings.HasSuffix(commit, "-dirty") {
		commit += "-dirty"
	}

	return version, commit, date
}

// pseudoVersion matches the "<14 digit timestamp>-<12 hex>" tail every
// pseudo-version ends with. The leading class is both separators Go uses: a
// hyphen with no base tag ("v0.0.0-2026...") and a dot with one
// ("v0.1.2-0.2026..."). One regexp beats a golang.org/x/mod dependency here.
var pseudoVersion = regexp.MustCompile(`[-.][0-9]{14}-[0-9a-f]{12}(\+[0-9a-z.]+)?$`)

// isReleaseVersion reports whether v is a version someone could go and fetch,
// as opposed to the toolchain's stand-in for an untagged or dirty build. A
// checkout build yields "0.1.2-0.20260920170255-3e60addac030+dirty", naming a
// release that does not exist, and that is worse than reporting "dev".
func isReleaseVersion(v string) bool {
	if v == "" || v == "(devel)" {
		return false
	}
	// "+dirty" is appended to a real tag too when the tree is modified, so the
	// build metadata has to be rejected separately from the pseudo-version.
	return !strings.Contains(v, "+") && !pseudoVersion.MatchString(v)
}

// shortCommit matches the width of goreleaser's .ShortCommit, so that the
// linked and the recovered paths print a commit of the same shape.
func shortCommit(rev string) string {
	const width = 7
	if len(rev) <= width {
		return rev
	}
	return rev[:width]
}

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
