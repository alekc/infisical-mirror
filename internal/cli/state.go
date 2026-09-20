package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/alekc/infisical-mirror/internal/config"
	"github.com/alekc/infisical-mirror/internal/state"
)

// defaultPruneAge is how long a tombstone is kept when nothing says otherwise.
// One only has to outlive the chance of the other side still holding the key;
// dropping it early makes the next pass read a surviving copy as a new secret
// and resurrect a deletion somebody meant. See docs/design.md.
const defaultPruneAge = 30 * 24 * time.Hour

const stateUsage = `Inspect or prune the sync state.

Usage:
  infisical-mirror state show  --config FILE [--scope SCOPE]
  infisical-mirror state prune --config FILE [--older-than DURATION] [--dry-run]
`

func runState(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, stateUsage)
		return ExitUsage
	}

	switch args[0] {
	case "show":
		return runStateShow(ctx, args[1:], stdout, stderr)
	case "prune":
		return runStatePrune(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, stateUsage)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "unknown state subcommand %q\n\n", args[0])
		fmt.Fprint(stderr, stateUsage)
		return ExitUsage
	}
}

func runStateShow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("state show", flag.ContinueOnError)
	fs.SetOutput(stderr)

	configPath := fs.String("config", "", "path to the configuration file (required)")
	scope := fs.String("scope", "", "list the entries of one scope instead of summarising every scope")

	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *configPath == "" {
		fmt.Fprintln(stderr, "state show: --config is required")
		return ExitUsage
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "state show: %v\n", err)
		return ExitError
	}

	// A shared lock: reading the state should never refuse an operator because
	// the scheduled pass is holding it, which is exactly when they are asking.
	store, err := openStore(cfg, state.ReadOnly())
	if err != nil {
		fmt.Fprintf(stderr, "state show: %v\n", err)
		return ExitError
	}
	defer store.Close()

	file, ok := store.(*state.FileStore)
	if !ok {
		fmt.Fprintln(stderr, "state show: this backend cannot be inspected")
		return ExitError
	}

	fmt.Fprintf(stdout, "state  %s\n", cfg.State.File.Path)
	// The fingerprint, never the salt. It is a one-way function of it, which is
	// enough to answer the only question anyone asks here: whether this file
	// was written under the salt this process is holding.
	fmt.Fprintf(stdout, "salt   fingerprint %s\n", state.Short(file.SaltFingerprint()))
	if cfg.State.SaltEnv != "" {
		fmt.Fprintf(stdout, "       supplied from $%s\n", cfg.State.SaltEnv)
	} else {
		fmt.Fprintln(stdout, "       generated and stored in the file itself")
	}
	fmt.Fprintln(stdout)

	scopes := file.Scopes()
	if len(scopes) == 0 {
		fmt.Fprintln(stdout, "no scopes recorded yet")
		return ExitOK
	}

	if *scope != "" {
		return showOneScope(ctx, store, *scope, stdout, stderr)
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SCOPE\tLIVE\tTOMBSTONED\tUPDATED")
	for _, name := range scopes {
		st, err := store.Load(ctx, name)
		if err != nil {
			fmt.Fprintf(stderr, "state show: %s: %v\n", name, err)
			return ExitError
		}
		live := st.Tracked()
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", name, live, len(st.Entries)-live, stampOrNever(st.UpdatedAt))
	}
	_ = tw.Flush()
	return ExitOK
}

// showOneScope lists a scope's entries. Hashes are truncated: a full keyed hash
// is not reversible, but it is an equality oracle for anyone who can compute
// one, and this command does not need to put a document's worth of them on a
// terminal, a CI log or a screen share.
func showOneScope(ctx context.Context, store state.Store, scope string, stdout, stderr io.Writer) int {
	st, err := store.Load(ctx, scope)
	if err != nil {
		fmt.Fprintf(stderr, "state show: %s: %v\n", scope, err)
		return ExitError
	}
	if st.FirstRun {
		fmt.Fprintf(stderr, "state show: no scope named %q; run `state show` with no --scope to list them\n", scope)
		return ExitError
	}

	fmt.Fprintf(stdout, "scope %s\n\n", scope)
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FOLDER\tKEY\tA\tB\tSYNCED")
	for _, key := range st.Keys() {
		entry, _ := st.Get(key)
		rel, name, ok := state.SplitEntryKey(key)
		if !ok {
			rel, name = "?", key
		}
		a, b := state.Short(entry.HashA), state.Short(entry.HashB)
		if entry.Tombstone {
			a, b = "deleted", "deleted"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", rel, name, a, b, stampOrNever(entry.SyncedAt))
	}
	_ = tw.Flush()
	return ExitOK
}

func runStatePrune(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("state prune", flag.ContinueOnError)
	fs.SetOutput(stderr)

	configPath := fs.String("config", "", "path to the configuration file (required)")
	olderThan := fs.Duration("older-than", defaultPruneAge, "drop tombstones last written longer ago than this")
	dryRun := fs.Bool("dry-run", false, "report what would be dropped and write nothing")

	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *configPath == "" {
		fmt.Fprintln(stderr, "state prune: --config is required")
		return ExitUsage
	}
	if *olderThan <= 0 {
		// Zero would drop every tombstone including the one written by the
		// pass that just ran, which is the case tombstones exist for.
		fmt.Fprintf(stderr, "state prune: --older-than must be positive, got %s\n", *olderThan)
		return ExitUsage
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "state prune: %v\n", err)
		return ExitError
	}

	var opts []state.FileOption
	if *dryRun {
		// The read-only store takes a shared lock and refuses Save, so a dry
		// run cannot write even if the code below were wrong about not asking
		// it to.
		opts = append(opts, state.ReadOnly())
	}
	store, err := openStore(cfg, opts...)
	if err != nil {
		fmt.Fprintf(stderr, "state prune: %v\n", err)
		return ExitError
	}
	defer store.Close()

	file, ok := store.(*state.FileStore)
	if !ok {
		fmt.Fprintln(stderr, "state prune: this backend cannot be pruned")
		return ExitError
	}

	cutoff := time.Now().Add(-*olderThan)
	total := 0
	for _, scope := range file.Scopes() {
		st, err := store.Load(ctx, scope)
		if err != nil {
			fmt.Fprintf(stderr, "state prune: %s: %v\n", scope, err)
			return ExitError
		}
		pruned := st.Prune(cutoff)
		if pruned == 0 {
			continue
		}
		total += pruned
		fmt.Fprintf(stdout, "%s: %d tombstone(s) older than %s\n", scope, pruned, *olderThan)

		if *dryRun {
			continue
		}
		if err := store.Save(ctx, scope, st); err != nil {
			fmt.Fprintf(stderr, "state prune: %s: %v\n", scope, err)
			return ExitError
		}
	}

	switch {
	case total == 0:
		fmt.Fprintf(stdout, "nothing to prune: no tombstone is older than %s\n", *olderThan)
	case *dryRun:
		fmt.Fprintf(stdout, "\n%d tombstone(s) would be dropped; nothing was written\n", total)
	default:
		fmt.Fprintf(stdout, "\n%d tombstone(s) dropped\n", total)
	}
	return ExitOK
}

func stampOrNever(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}
