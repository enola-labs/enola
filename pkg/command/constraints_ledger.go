package command

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/check"
)

// ConstraintsLedger is `enola constraints ledger [--json] [repo_path]`: how
// much of the declared law is being excused rather than obeyed.
//
// The gate has always collected the inputs — a suppression entry and an
// `exempt:` carve-out each name an owner, a reason and a date — and never
// asked the question they answer. A per-run verdict reports each excuse in
// isolation, which is exactly the view in which a rule that everybody signs
// away looks like thirty separate reasonable decisions.
//
// Read from the SNAPSHOT's own intent facts rather than from the working
// tree's YAML, deliberately: the rules counted here are the rules that
// produced the findings being counted beside them, so the two halves of every
// ratio come from one state of the law. A rule edited since the snapshot is
// therefore absent, and the report prints the snapshot's age so a reader can
// tell.
//
// A report, never a gate: exit 0 whenever a report was produced, 2 when it
// could not run. Nothing here changes what `enola check` fails on — a gate
// that failed on its own unpopularity is one nobody would leave enabled.
func (r *Runner) ConstraintsLedger(args []string) {
	fs := flag.NewFlagSet("constraints ledger", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "print the ledger as JSON")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: "+r.name()+" constraints ledger [--json] [repo_path]\n\n"+
			"Every declared rule, its enforcement mode, the breaches it raised in the\n"+
			"current snapshot, and how many of those a suppression or an exemption\n"+
			"excused — with each excuse's owner, reason and age.\n\n"+
			"The rate is evidence for a human: a rule whose breaches are mostly signed\n"+
			"away is a rule to reconsider, and that is invisible one verdict at a time.\n"+
			"A report, never a gate — nothing here fails a build.\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, "constraints ledger")
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(check.StatusUsageError.ExitCode())
	}
	repoPath := "."
	if fs.NArg() > 0 {
		repoPath = fs.Arg(0)
	}
	repoPath, _ = filepath.Abs(repoPath)

	tgt := r.resolveTarget(repoPath)
	tgt.engine.SetPersistCache(false)
	snap, err := bootstrap.LoadSnapshotDir(tgt.engine.OutputDir(repoPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "constraints ledger: no snapshot for %s; generate one first\n", repoPath)
		os.Exit(check.StatusUsageError.ExitCode())
	}
	// A malformed ledger is an operational failure here exactly as it is in
	// `check`: reporting an excuse rate from half an excuse list would understate
	// the number this command exists to state.
	suppressions, err := check.LoadSuppressions(repoPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "constraints ledger: %v\n", err)
		os.Exit(check.StatusUsageError.ExitCode())
	}
	store := facts.NewStore()
	store.Add(snap.Facts...)
	led := check.ComputeLedger(store, suppressions, snap.Insights, time.Now())

	// Staleness, computed the way `plan` computes it. Without it this report
	// and `enola check` can print different numbers from the same tree and
	// neither says why: check re-extracts on every run, while this reads the
	// snapshot on disk, so an edit since the last snapshot moves one and not the
	// other. A report that silently disagrees with the gate is worse than no
	// report.
	staleness := ""
	if err := tgt.engine.RestoreFromDir(tgt.engine.OutputDir(repoPath),
		map[string]string{filepath.Base(repoPath): repoPath}, filepath.Base(repoPath)); err == nil {
		if d, dErr := tgt.engine.DriftFromMeta(repoPath, snap.Meta); dErr == nil && (d.Unknown || d.Any()) {
			staleness = d.Summary(5)
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			check.Ledger
			GeneratedAt string `json:"generated_at,omitempty"`
			Staleness   string `json:"staleness,omitempty"`
		}{led, snap.Meta.GeneratedAt, staleness})
		os.Exit(0)
	}
	fmt.Print(renderLedger(led, snap.Meta, staleness))
	os.Exit(0)
}

// renderLedger prints the summary line, then a row per rule that has something
// to say. Rules with no breaches and no excuses are counted rather than listed:
// they are the denominator, and printing thirty quiet rows is how a reader
// stops reaching the four that matter.
func renderLedger(led check.Ledger, meta facts.SnapshotMeta, staleness string) string {
	var sb strings.Builder
	if led.Summary.Rules == 0 {
		sb.WriteString("No rules declared — nothing to excuse.\n\n" +
			"This repository has not stated a law, so it is unasked rather than clean.\n" +
			"See docs/CONSTRAINTS.md to declare one.\n")
		return sb.String()
	}
	fmt.Fprintf(&sb, "%s\n", led.Summary.Line())
	switch {
	case staleness != "":
		fmt.Fprintf(&sb, "read from the snapshot generated %s, which is STALE relative to the working tree: %s\n"+
			"Re-snapshot before trusting these counts — `enola check` re-extracts and will not agree with them.\n",
			meta.GeneratedAt, staleness)
	case meta.GeneratedAt != "":
		fmt.Fprintf(&sb, "read from the snapshot generated %s — a rule declared since then is not counted here\n",
			meta.GeneratedAt)
	}

	rules := append([]check.LedgerRule(nil), led.Rules...)
	sort.SliceStable(rules, func(i, j int) bool {
		a, b := rules[i], rules[j]
		if ea, eb := a.Suppressed+a.Exempted, b.Suppressed+b.Exempted; ea != eb {
			return ea > eb
		}
		if a.Breaches != b.Breaches {
			return a.Breaches > b.Breaches
		}
		return a.ID < b.ID
	})

	quiet := 0
	shown := 0
	for _, rule := range rules {
		if rule.Breaches == 0 && rule.Exempted == 0 && len(rule.Excuses) == 0 {
			quiet++
			continue
		}
		if shown == 0 {
			sb.WriteString("\n")
		}
		shown++
		mode := rule.Mode
		if mode == "" {
			mode = "ratchet"
		}
		fmt.Fprintf(&sb, "%s [%s] — %s reported, %d excused",
			rule.ID, mode, countOf(rule.Breaches, "breach", "breaches"), rule.Suppressed+rule.Exempted)
		if rule.Source != "" {
			fmt.Fprintf(&sb, " · declared in %s", rule.Source)
		}
		sb.WriteString("\n")
		if rule.Because != "" {
			fmt.Fprintf(&sb, "    because: %s\n", rule.Because)
		}
		for _, ex := range rule.Excuses {
			age := "undated"
			if ex.Date != "" {
				age = ex.Date
				if ex.AgeDays > 0 {
					age = fmt.Sprintf("%s, %s ago", ex.Date, countOf(ex.AgeDays, "day", "days"))
				}
			}
			owner := ex.Owner
			if owner == "" {
				owner = "unnamed"
			}
			fmt.Fprintf(&sb, "    %s by %s (%s)", ex.Kind, owner, age)
			if ex.Witness != "" {
				fmt.Fprintf(&sb, " — %s", ex.Witness)
			}
			// An idle excuse is named on its own line rather than dropped: a
			// signature that stopped excusing anything is the one row in this
			// report someone can act on today.
			if !ex.Matched {
				sb.WriteString(" [matched nothing in this snapshot]")
			}
			sb.WriteString("\n")
			if ex.Reason != "" {
				fmt.Fprintf(&sb, "        %q\n", ex.Reason)
			}
		}
	}
	if quiet > 0 {
		fmt.Fprintf(&sb, "\n%s with no breaches and no excuses.\n", countOf(quiet, "rule", "rules"))
	}
	if led.Summary.Excused > 0 {
		sb.WriteString("\nA rule whose breaches are mostly excused is a rule to reconsider, not a\n" +
			"team to chase: the excuse rate is what makes that judgement available.\n")
	}
	return sb.String()
}

// countOf renders a count with an explicit plural form. The package's plural()
// appends an "s", which is right for "rule" and wrong for "breach".
func countOf(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}
