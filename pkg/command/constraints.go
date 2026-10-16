package command

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/enola-labs/enola/internal/explainers/constraints"
	"github.com/enola-labs/enola/internal/explainers/layers"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/check"
)

// Constraints is `enola constraints <lint|mine|init|explain|ledger>` — the
// authoring loop for the declared-constraint vocabulary. `mine` (see
// ConstraintsMine) proposes candidate declarations out of the snapshot's own
// regularities; `ledger` (see ConstraintsLedger) reports how much of the law
// already declared is being excused rather than obeyed; `lint`
// parses each repo's declaration
// (enola-intent.yaml, any enola/constraints/*.yaml files, and any
// cluster-config override), reports every
// validation problem with its file context, and — when a snapshot exists on
// disk — resolves each declared component against it so an author sees what a
// selector actually selects BEFORE a rule built on it verdicts anything. It
// never generates a snapshot and never writes: a missing snapshot degrades to
// validation-only, named as such.
//
// Exit codes follow the gate's contract where it overlaps: 0 when every
// declaration is valid, 1 when any validation problem was reported, 2 when the
// command could not run at all.
func (r *Runner) Constraints(args []string) {
	if len(args) > 0 && args[0] == "mine" {
		r.ConstraintsMine(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "init" {
		r.ConstraintsInit(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "explain" {
		r.ConstraintsExplain(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "ledger" {
		r.ConstraintsLedger(args[1:])
		return
	}
	fs := flag.NewFlagSet("constraints", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: "+r.name()+" constraints <lint|mine|init|explain|ledger> [repo_path|config_path]\n\n"+
			"lint validates the declared constraint vocabulary — inline in enola-intent.yaml\n"+
			"and per-domain files under enola/constraints/ — and resolves each component\n"+
			"against the current snapshot, if one exists (else validation only).\n\n"+
			"mine searches the current snapshot's fact store for near-invariants and\n"+
			"reports them as candidate constraint declarations — proposals for operator\n"+
			"review, never self-adopting law. Run `"+r.name()+" constraints mine --help`.\n"+
			"init writes a first declaration binding every shipped recipe whose roles\n"+
			"resolve to directories the repository has. Run `"+r.name()+" constraints init --help`.\n"+
			"explain names the components a file's facts belong to, the selector that\n"+
			"admitted each, and the edges the file makes. Run `"+r.name()+" constraints explain --help`.\n"+
			"ledger reports how much of the declared law is being EXCUSED rather than\n"+
			"obeyed — every rule's breaches beside the suppressions and exemptions that\n"+
			"signed them away, with each excuse's owner and age. Run `"+r.name()+" constraints ledger --help`.\n\n"+
			"Exit codes (lint):\n"+
			"  0  every declaration is valid\n"+
			"  1  validation problems were reported\n"+
			"  2  the command could not run\n\n"+
			"Exit codes (mine, init, explain, ledger):\n"+
			"  0  a report was produced\n"+
			"  2  the command could not run — no snapshot to read, or a declaration\n"+
			"     init would have had to overwrite\n")
		r.writeHelpFooter(os.Stderr, args, "constraints")
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(check.StatusUsageError.ExitCode())
	}
	rest := fs.Args()
	if len(rest) == 0 || rest[0] != "lint" || HelpRequested(rest) {
		fs.Usage()
		os.Exit(check.StatusUsageError.ExitCode())
	}

	var arg string
	if len(rest) > 1 {
		arg = rest[1]
	}
	tgt := r.resolveTarget(arg)
	fmt.Fprintf(os.Stderr, r.name()+" constraints lint: %s\n", tgt.configNote)
	// Read-only like check and coverage: linting a declaration must not touch
	// the cache, the snapshot, or anything else on disk.
	tgt.engine.SetPersistCache(false)

	problems := 0
	declared := facts.NewStore()
	for _, repoPath := range tgt.repoPaths {
		problems += r.lintRepoDeclaration(tgt.engine.Config().Intent[filepath.Base(repoPath)], repoPath, declared)
	}

	problems += r.lintResolveComponents(tgt.engine, tgt.repoPaths[0], declared)

	if problems > 0 {
		fmt.Printf("\nFAIL — %s.\n", plural(problems, "validation problem"))
		os.Exit(1)
	}
	fmt.Println("\nOK — every declaration is valid.")
	os.Exit(0)
}

// lintRepoDeclaration reports one repo's declaration sources — the inline
// file, each enola/constraints file with its own counts, and any cluster
// entry — and their validation problems, compiles whatever resolved cleanly
// into declared for the component pass, and returns the problem count.
// Constraints-file problems come out of the merged declaration already citing
// their file, so each is printed under the file that declared it.
func (r *Runner) lintRepoDeclaration(clusterDecl *intent.Declaration, repoPath string, declared *facts.Store) int {
	label := filepath.Base(repoPath)

	filePath := filepath.Join(repoPath, intent.RepoFileName)
	var fileDecl *intent.Declaration
	fileProblems := []string{}
	switch data, err := os.ReadFile(filePath); {
	case os.IsNotExist(err):
	case err != nil:
		r.constraintsFatal("reading %s: %v", filePath, err)
	default:
		var d intent.Declaration
		if yamlErr := yaml.Unmarshal(data, &d); yamlErr != nil {
			fileProblems = append(fileProblems, fmt.Sprintf("not parseable as YAML: %v", yamlErr))
		} else {
			d.Normalize()
			d.Source = intent.RepoFileName
			fileDecl = &d
		}
	}

	dirFiles, dirProblems, err := intent.LoadConstraintsDir(repoPath)
	if err != nil {
		r.constraintsFatal("%v", err)
	}
	recipes, recipeFileProblems, err := intent.LoadRecipesDir(repoPath)
	if err != nil {
		r.constraintsFatal("%v", err)
	}
	recipes, builtinNotes := intent.MergeBuiltinRecipes(recipes)
	recipeProblems, recipeWarnings := intent.RecipeProblems(recipes)
	// A repository authoring a recipe the binary also ships is exercising the
	// override, not making a mistake, so the lint surface says which one ran
	// and nothing fails.
	recipeWarnings = append(recipeWarnings, builtinNotes...)
	recipeWarnings = append(recipeWarnings, intent.UnboundOptionalRules(recipes, dirFiles)...)

	hasFile := fileDecl != nil || len(fileProblems) > 0
	hasDir := len(dirFiles) > 0 || len(dirProblems) > 0
	hasRecipes := len(recipes) > 0 || len(recipeFileProblems) > 0
	switch {
	case !hasFile && !hasDir && !hasRecipes && clusterDecl == nil:
		fmt.Printf("\n%s: no declaration (%s absent, no %s/ files, no cluster intent entry) — nothing to lint.\n", label, intent.RepoFileName, intent.ConstraintsDirName)
		return 0
	case !hasFile && !hasDir && clusterDecl == nil:
		fmt.Printf("\n%s: recipe definitions under %s/ only — nothing declares or instantiates them yet.\n", label, intent.RecipesDirName)
	case clusterDecl != nil && (hasFile || hasDir):
		fmt.Printf("\n%s: cluster config overrides the repo's declaration wholesale; both are linted, the cluster entry governs.\n", label)
	case clusterDecl != nil:
		fmt.Printf("\n%s: declared by the cluster config's intent entry.\n", label)
	case hasFile && hasDir:
		fmt.Printf("\n%s: declared by %s plus %s under %s/.\n", label, intent.RepoFileName, plural(len(dirFiles), "constraints file"), intent.ConstraintsDirName)
	case hasDir:
		fmt.Printf("\n%s: declared by constraints files under %s/.\n", label, intent.ConstraintsDirName)
	default:
		fmt.Printf("\n%s: declared by %s.\n", label, intent.RepoFileName)
	}

	count := 0
	if fileDecl != nil {
		fmt.Printf("  %s: %d component(s), %d rule(s), %d exemption(s)\n", filePath, len(fileDecl.Components), len(fileDecl.Rules), exemptionCount(fileDecl.Rules))
	}
	recipeByName := map[string]intent.Recipe{}
	for _, rec := range recipes {
		if _, seen := recipeByName[rec.Name]; !seen {
			recipeByName[rec.Name] = rec
		}
	}
	for _, f := range dirFiles {
		fmt.Printf("  %s: %d component(s), %d rule(s), %d exemption(s)\n", f.Path, len(f.Components), len(f.Rules), exemptionCount(f.Rules))
		for _, inst := range f.UseRecipe {
			expanded := 0
			if rec, found := recipeByName[inst.Recipe]; found {
				expanded = len(rec.Rules)
			}
			binds := make([]string, 0, len(inst.Bind))
			for role := range inst.Bind {
				binds = append(binds, role)
			}
			sort.Strings(binds)
			bound := "nothing"
			if len(binds) > 0 {
				bound = strings.Join(binds, ", ")
			}
			fmt.Printf("    use_recipe %s (recipe %s): binds %s, expands %s\n", inst.As, inst.Recipe, bound, plural(expanded, "rule"))
		}
	}
	for _, rec := range recipes {
		roles := make([]string, 0, len(rec.Roles))
		for _, role := range rec.Roles {
			roles = append(roles, role.Name)
		}
		fmt.Printf("  %s: recipe %s — roles %s, %s\n", rec.Path, rec.Name, strings.Join(roles, ", "), plural(len(rec.Rules), "rule"))
	}
	report := func(source string, list []string) {
		for _, p := range list {
			fmt.Printf("  %s: %s\n", source, p)
			count++
		}
	}
	report(filePath, fileProblems)
	for _, p := range dirProblems {
		fmt.Printf("  %s\n", p)
		count++
	}
	for _, p := range recipeFileProblems {
		fmt.Printf("  %s\n", p)
		count++
	}
	for _, p := range recipeProblems {
		fmt.Printf("  %s\n", p)
		count++
	}
	for _, w := range recipeWarnings {
		fmt.Printf("  %s\n", w)
	}

	merged := intent.MergeConstraintsFiles(fileDecl, dirFiles)
	merged, expandProblems := intent.ApplyRecipes(merged, dirFiles, recipes)
	for _, p := range expandProblems {
		fmt.Printf("  %s\n", p)
		count++
	}
	if merged != nil {
		for _, p := range merged.Problems() {
			if strings.HasPrefix(p, intent.ConstraintsDirName+"/") {
				fmt.Printf("  %s\n", p)
			} else {
				fmt.Printf("  %s: %s\n", filePath, p)
			}
			count++
		}
	}
	if clusterDecl != nil {
		// Cluster entries were already validated when the config loaded — an
		// invalid one never reaches this command — so Problems here is a
		// belt-and-braces re-check that also keeps the report symmetric.
		fmt.Printf("  cluster intent entry %s: %d component(s), %d rule(s), %d exemption(s)\n", label, len(clusterDecl.Components), len(clusterDecl.Rules), exemptionCount(clusterDecl.Rules))
		report("cluster intent entry "+label, clusterDecl.Problems())
	}

	resolved := intent.Resolve(merged, clusterDecl)
	if resolved != nil && len(resolved.Problems()) == 0 {
		declared.Add(intent.CompileFacts(resolved)...)
	}
	return count
}

// lintResolveComponents joins the declared components against the snapshot on
// disk, if any: measured facts from the snapshot, component and rule facts
// from the declarations as they stand NOW — the file being edited, not the
// copy compiled into the snapshot — so the counts answer for the author's
// working tree. It returns the number of problems the join found. A selector
// naming a property the snapshot does not measure is one, because a predicate
// nothing can evaluate is a broken declaration rather than an empty component.
// It fails here, at authoring time, rather than being read as law from its own
// silence. A predicate component in an edge-walking role needs no snapshot to
// refuse: it is a defect of the declaration alone, and the validation pass
// above has already reported it.
// pageCompiled reports whether an intent fact was compiled from a knowledge
// page rather than from a declaration file. Pages are the one intent carrier
// the working tree's declaration cannot reproduce, so these facts survive the
// swap that re-reads components and rules from disk.
func pageCompiled(f facts.Fact) bool {
	switch f.PropString("intent_kind") {
	case "page", "anchor", "relation":
		return true
	}
	return false
}

// lintStore builds the store component resolution is reported against: every
// measured fact from the snapshot, the intent facts compiled from knowledge
// pages, and the DECLARATION read fresh from the working tree.
//
// The snapshot's own declaration facts are dropped so an edit to
// enola-intent.yaml or a constraints file is linted without regenerating.
// Page-compiled facts are not part of any declaration file and are kept: a
// component selecting by governed_by resolves through exactly those, and
// dropping them reported such a component as `0 member(s) — matches nothing;
// every rule naming it holds vacuously` while the explainer resolved its
// members and reported breaches against them. A lint that contradicts the
// verdict it exists to preview is worse than no lint.
//
// Declared facts are stamped with the snapshot's repo label before they join.
// The engine does the same thing (SetRepoRange over the extraction window),
// and a declared layer is resolved against the modules of the repo that OWNS
// it — so unlabelled intent facts beside labelled module facts resolve to
// nothing at all, which is precisely the silent-empty answer this report
// exists to expose.
func lintStore(measured []facts.Fact, declared []facts.Fact, label string) *facts.Store {
	store := facts.NewStore()
	for _, f := range measured {
		if f.Kind == facts.KindIntent && !pageCompiled(f) {
			continue
		}
		store.Add(f)
	}
	if label != "" {
		for i := range declared {
			if declared[i].Repo == "" {
				declared[i].Repo = label
			}
		}
	}
	store.Add(declared...)
	return store
}

func (r *Runner) lintResolveComponents(eng *bootstrap.Engine, anchor string, declared *facts.Store) int {
	if len(declared.ByKind(facts.KindIntent)) == 0 {
		return 0
	}
	outDir := eng.OutputDir(anchor)
	snap, err := bootstrap.LoadSnapshotDir(outDir)
	if err != nil {
		fmt.Printf("\nComponent resolution: no snapshot at %s - validation only.\n", outDir)
		return 0
	}
	store := lintStore(snap.Facts, declared.ByKind(facts.KindIntent), snapshotLabel(snap))

	unevaluableList := constraints.UnevaluableSelectors(store)
	unevaluable := map[string]bool{}
	for _, u := range unevaluableList {
		unevaluable[u.Component] = true
	}
	unasked := constraints.UnaskedComponents(store)

	fmt.Printf("\nComponent resolution against the snapshot at %s:\n", outDir)
	for _, c := range constraints.MemberCounts(store) {
		note := ""
		switch {
		case unasked[c.Component] != "":
			note = "  <- names service " + unasked[c.Component] + ", absent from this snapshot; unasked, never failed"
		case unevaluable[c.Component]:
			note = "  <- selector cannot be evaluated against this snapshot"
		case c.Members == 0:
			note = "  <- matches nothing; every rule naming it holds vacuously"
		}
		fmt.Printf("  %-24s %d member(s)%s\n", c.Component, c.Members, note)
		if c.Selector != "" {
			fmt.Printf("  %-24s   %s\n", "", c.Selector)
		}
	}
	if len(unevaluableList) > 0 {
		fmt.Printf("\nSelectors this snapshot cannot evaluate:\n")
		for _, u := range unevaluableList {
			suggestion := ""
			if len(u.NearMiss) > 0 {
				suggestion = fmt.Sprintf(" (measured properties with similar names: %s)", strings.Join(u.NearMiss, ", "))
			}
			fmt.Printf("  %s: %s%s — declared in %s\n", u.Component, u.Problem(), suggestion, u.Source)
		}
	}
	unreachable := constraints.UnreachableRoles(store)
	if len(unreachable) > 0 {
		fmt.Printf("\nRoles this snapshot resolves against nothing:\n")
		for _, u := range unreachable {
			verdict := "the rule emits no verdict"
			if u.Partial {
				verdict = "the rule's other edge kinds still verdict"
			}
			fmt.Printf("  %s: %s — %s\n", u.Rule, u.Problem(), verdict)
		}
	}
	if absent := constraints.AbsentExemplars(store); len(absent) > 0 {
		fmt.Printf("\nGuidance exemplars the snapshot cannot resolve (a note, not an error):\n")
		for _, n := range absent {
			fmt.Printf("  %s: %s\n", n.Rule, n.Exemplar)
		}
	}
	reportLayerResolution(store)
	return len(unevaluableList)
}

// reportLayerResolution prints what each declared layer selects, beside the
// component counts above it.
//
// Layers are resolved here for the same reason components are: a declaration is
// only worth what it selects, and the moment to learn that it selects nothing is
// while it is being written, not three snapshots later when a gate that was never
// going to fire keeps exiting 0. It is not counted as a lint PROBLEM — a layer
// order matching nothing is a mistake but not an invalid declaration, and the
// explainer already raises an advisory for it every snapshot.
func reportLayerResolution(store *facts.Store) {
	counts := layers.MemberCounts(store)
	if len(counts) == 0 {
		return
	}
	fmt.Printf("\nDeclared layer resolution (outermost first):\n")
	empty := 0
	lastRepo := ""
	for _, c := range counts {
		if c.Repo != lastRepo {
			if c.Repo != "" {
				fmt.Printf("  %s:\n", c.Repo)
			}
			lastRepo = c.Repo
		}
		note := ""
		if c.Members == 0 {
			note = "  <- matches nothing; no import can violate this layer"
			empty++
		}
		fmt.Printf("  %-24s %d module(s)%s\n", c.Layer, c.Members, note)
		fmt.Printf("  %-24s   %s\n", "", strings.Join(c.Paths, " "))
	}
	if empty == 0 {
		return
	}
	// The measured module paths, once, at the end: a declared path and the path it
	// was meant to match are nearly always one visible edit apart, and without this
	// the author has to go query the fact store to see the second one.
	for _, repo := range dedupeRepos(counts) {
		names := layers.ModuleNames(store, repo)
		if len(names) == 0 {
			continue
		}
		if len(names) > 12 {
			names = append(names[:12:12], fmt.Sprintf("... and %d more", len(layers.ModuleNames(store, repo))-12))
		}
		label := "module paths this snapshot measured"
		if repo != "" {
			label = repo + ": " + label
		}
		fmt.Printf("\n  %s:\n    %s\n", label, strings.Join(names, "\n    "))
	}
}

// dedupeRepos lists the repos named by a layer-count set, in first-seen order.
func dedupeRepos(counts []layers.LayerCount) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range counts {
		if seen[c.Repo] {
			continue
		}
		seen[c.Repo] = true
		out = append(out, c.Repo)
	}
	return out
}

// snapshotLabel names the repo label the snapshot's facts carry. The recorded
// label wins; snapshots written before RepoLabel existed fall back to the label
// their own facts were tagged with, which is what those builds used.
func snapshotLabel(snap *facts.Snapshot) string {
	if snap == nil {
		return ""
	}
	if snap.Meta.RepoLabel != "" {
		return snap.Meta.RepoLabel
	}
	for _, f := range snap.Facts {
		if f.Repo != "" {
			return f.Repo
		}
	}
	return ""
}

func exemptionCount(rules []intent.ConstraintRule) int {
	n := 0
	for _, rule := range rules {
		n += len(rule.Exempt)
	}
	return n
}

func (r *Runner) constraintsFatal(format string, args ...any) {
	r.cmdFatal("constraints", format, args...)
}
