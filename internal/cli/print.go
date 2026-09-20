package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/alekc/infisical-mirror/internal/config"
	"github.com/alekc/infisical-mirror/internal/reconcile"
)

// printPlan renders one rule's plan. Nothing here prints a secret value: the
// only thing it has access to is Action, which keeps its value unexported for
// exactly this reason.
func printPlan(w io.Writer, p *reconcile.Plan) {
	header := fmt.Sprintf("rule %s  %s %s %s", p.Pair.Rule, p.Pair.A.Env, arrow(p.Pair.Mode), p.Pair.B.Env)
	if p.Pair.DryRun {
		header += "  [dry run]"
	}
	fmt.Fprintln(w, header)
	fmt.Fprintf(w, "  a  %s  %d secret(s) in scope\n", p.Pair.A, p.SecretsA)
	fmt.Fprintf(w, "  b  %s  %d secret(s) in scope\n", p.Pair.B, p.SecretsB)

	if p.FirstRun {
		fmt.Fprintln(w, "  state: first run for this pair, so no key can be told from a new one")
	} else {
		// Tracked and InScope differ once a rule is narrowed: the file still
		// holds entries for folders it no longer selects, and the guards
		// measure against the second. Printing only the first made the guard's
		// arithmetic unreproducible from its own output.
		fmt.Fprintf(w, "  state: %d key(s) tracked", p.Tracked)
		if p.InScope != p.Tracked {
			fmt.Fprintf(w, ", %d of them in scope for this rule", p.InScope)
		}
		fmt.Fprintln(w)
	}

	if len(p.Actions) == 0 {
		fmt.Fprintln(w, "  nothing to do")
	} else {
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, a := range p.Actions {
			side := string(a.Side)
			if side == "" {
				side = "-"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", a.Op, side, a.RelPath, a.Key, a.Reason)
		}
		_ = tw.Flush()
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "  %s\n", summary(p))
	for _, g := range p.Guards {
		fmt.Fprintf(w, "  REFUSED  %s\n", g)
	}
	fmt.Fprintln(w)
}

// summary counts the plan in one line, leaving out whatever is zero.
func summary(p *reconcile.Plan) string {
	counts := []struct {
		n     int
		label string
	}{
		{count(p, reconcile.OpCreate), "to create"},
		{count(p, reconcile.OpUpdate), "to update"},
		{count(p, reconcile.OpDelete), "to delete"},
		{count(p, reconcile.OpConflict), "conflict(s)"},
		{count(p, reconcile.OpDeleteObserved), "deletion(s) reported"},
		{p.Converged, "converged"},
		{p.Untracked, "untracked"},
		{p.OutOfScope, "out of scope"},
		{p.Retired, "retired from state"},
	}

	var parts []string
	for _, c := range counts {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.label))
		}
	}
	if len(parts) == 0 {
		return "no secrets in scope on either side"
	}
	return strings.Join(parts, ", ")
}

func count(p *reconcile.Plan, op reconcile.Op) int {
	n := 0
	for _, a := range p.Actions {
		if a.Op == op {
			n++
		}
	}
	return n
}

func arrow(m config.Mode) string {
	switch m {
	case config.ModeAToB:
		return "->"
	case config.ModeBToA:
		return "<-"
	default:
		return "<->"
	}
}
