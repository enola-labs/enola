package server

// The tools that answer from declared intent: governing_intent, constraints_for
// and plan_check.

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/constraints"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plan"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerGoverningIntentTool registers the governing_intent tool.
func (s *Server) registerGoverningIntentTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "governing_intent",
		Description: "Knowledge to code and back. For a fact or file: the knowledge pages whose anchors cover its file, each with type, status and outgoing relations, so the trail continues past the first hop. " +
			"For a compiled page path: its anchors with measured coverage (files and facts under each). " +
			"Cheaper than impact_analysis when the question is governance, not blast radius.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args governingIntentArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		graph := store.Graph()
		if graph == nil {
			return errorResult("No graph available. Run generate_snapshot first."), nil, nil
		}
		if args.Target == "" {
			return errorResult("target is required"), nil, nil
		}
		if !graph.HasCompiledPages() {
			return textResult("No knowledge pages are compiled into this snapshot — the reverse query was not asked. " +
				"Pages opt in with an enola_intent page declaration and compile at snapshot time; see docs/INTENT.md."), nil, nil
		}

		if cov, found := graph.GovernedByPage(args.Target); found {
			return textResult(capTokens(renderGovernedCode(args.Target, cov), args.MaxTokens, false)), nil, nil
		}

		type site struct{ repo, file string }
		var sites []site
		seen := map[site]bool{}
		add := func(repo, file string) {
			st := site{repo, file}
			if repo != "" && file != "" && !seen[st] {
				seen[st] = true
				sites = append(sites, st)
			}
		}
		for _, m := range store.LookupByExactName(args.Target) {
			if m.Kind == facts.KindIntent || (args.Repo != "" && m.Repo != args.Repo) {
				continue
			}
			add(m.Repo, m.File)
		}
		if len(sites) == 0 {
			for _, f := range store.All() {
				if f.Kind == facts.KindIntent || f.File == "" || f.Repo == "" ||
					(args.Repo != "" && f.Repo != args.Repo) {
					continue
				}
				if f.File == args.Target || strings.TrimPrefix(f.File, f.Repo+"/") == args.Target {
					add(f.Repo, f.File)
				}
			}
		}
		if len(sites) == 0 {
			return textResult(fmt.Sprintf("Nothing measured matches %q — not a compiled page, an exact fact name, or a measured file path. "+
				"File paths join in the label-prefixed and repo-relative forms; pass repo= to scope one.", args.Target)), nil, nil
		}
		if len(sites) > 5 {
			sites = sites[:5]
		}

		var sb strings.Builder
		for i, st := range sites {
			if i > 0 {
				sb.WriteString("\n---\n\n")
			}
			fmt.Fprintf(&sb, "### %s (%s)\n\n", st.file, st.repo)
			pages := graph.GoverningIntentForFile(st.repo, st.file)
			if len(pages) == 0 {
				sb.WriteString("No knowledge page anchors this file — asked, none governs.\n")
				continue
			}
			writeGoverningIntent(&sb, pages)
		}
		return textResult(capTokens(sb.String(), args.MaxTokens, false)), nil, nil
	})
}

// registerConstraintsForTool registers the constraints_for tool.
func (s *Server) registerConstraintsForTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "constraints_for",
		Description: "The pre-edit contract for a file or fact; ask BEFORE editing. Returns the constraint components that contain it, every rule binding those components (in words, with its declared rationale and enforcement mode), and the current violations whose evidence names it. " +
			"Guidance rules (steering, not law) come with their advice, mode, and prior-art exemplars marked present, absent or unmeasured in this snapshot. " +
			"The file may not exist yet: it is matched against component patterns, so the answer covers code about to be written. Matching is exact and fail-closed, like the constraints explainer's. Returns JSON.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args constraintsForArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		if args.Target == "" {
			return errorResult("target is required"), nil, nil
		}
		target := s.normalizeToRelative(args.Target)
		bindings, declared := constraints.ContractFor(store, target)
		if !declared {
			return textResult("No constraint components are compiled into this snapshot — the contract was not asked. " +
				"Components and rules ride enola_intent page declarations and compile at snapshot time; see docs/INTENT.md."), nil, nil
		}
		resp := constraintsForResponse{Target: target, Components: bindings}
		if snap := s.eng.Snapshot(); snap != nil {
			resp.Violations = constraints.ViolationsReferencing(snap.Insights, target)
		}
		return jsonResultCapped(resp, args.MaxTokens)
	})
}

// registerPlanCheckTool registers the plan_check tool.
func (s *Server) registerPlanCheckTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "plan_check",
		Description: "Plan a change BEFORE editing. For each target (paths that may not exist yet, or exact fact names): the constraint components and rules governing it, with mode and declared because:, plus its blast radius (fan-in and fan-out, capped samples with exact counts). " +
			"With patch, the counterfactual: the diff is applied to a scratch copy (the working tree and its .enola are never touched), facts are regenerated there, and the constraint verdicts that WOULD appear are reported as new, resolved or unchanged, each naming the rule, the would-be witness and its because:. " +
			"Components and rules are read from the working tree (enola-intent.yaml, enola/constraints/), so an edit to them counts without regenerating. " +
			"A report, not a gate: like enola check, the verdict is the caller's to weigh. Returns JSON.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args planCheckArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		if len(args.Paths) == 0 && len(args.Symbols) == 0 && args.Patch == "" {
			return errorResult("nothing to plan: give paths, symbols, or patch"), nil, nil
		}
		target, err := s.planTarget(store, args)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		repoPath, label, paths := target.repoPath, target.label, target.paths
		measured := store.All()
		if target.multi {
			// Only this repository's facts: a repo-relative target would otherwise match
			// the same relative path in every other member.
			own := measured[:0:0]
			for _, f := range measured {
				if f.Repo == label {
					own = append(own, f)
				}
			}
			measured = own
		}
		contractStore, err := plan.ContractStore(repoPath, measured, s.eng.Config().Intent[label])
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		info := plan.SnapshotInfo{}
		if snap := s.eng.Snapshot(); snap != nil {
			info.GeneratedAt = snap.Meta.GeneratedAt
		}
		if d, driftErr := s.driftOf(repoPath); driftErr == nil && (d.Unknown || d.Any()) {
			info.Staleness = d.Summary(5)
		}
		deps := plan.Deps{
			RepoPath:      repoPath,
			RepoLabel:     label,
			Store:         contractStore,
			Snapshot:      info,
			OutputDirName: s.eng.Config().Output.Dir,
			NewEngine:     s.planFactory,
		}
		report, err := plan.Compute(ctx, plan.Request{Paths: paths, Symbols: args.Symbols, Patch: []byte(args.Patch)}, deps)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		return jsonResultCapped(report, args.MaxTokens)
	})
}

// planScope is the one repository a plan_check call is about, and its targets
// relative to that repository's root.
type planScope struct {
	repoPath, label string
	paths           []string
	multi           bool
}

// planTarget resolves which repository a plan_check call is about. A plan is one
// repository's: its declarations govern it and a patch applies to its tree. In a
// multi-repo snapshot that repository used to be the primary one, whichever was
// indexed last, whatever the targets named, so a golf/ path was planned against another
// member's rules and drift. Now label-prefixed paths and fact names select it, repo
// names it outright, and targets in two repositories are refused rather than guessed.
func (s *Server) planTarget(store *facts.Store, args planCheckArgs) (planScope, error) {
	repos := s.eng.RepoPaths()
	if len(repos) <= 1 {
		repoPath := s.currentRepoPath()
		if repoPath == "" {
			return planScope{}, fmt.Errorf("no snapshot available; run generate_snapshot first")
		}
		paths := make([]string, 0, len(args.Paths))
		for _, p := range args.Paths {
			paths = append(paths, s.normalizeToRelative(p))
		}
		return planScope{repoPath: repoPath, label: filepath.Base(repoPath), paths: paths}, nil
	}

	labels := make([]string, 0, len(repos))
	for l := range repos {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	if args.Repo != "" {
		if _, ok := repos[args.Repo]; !ok {
			return planScope{}, fmt.Errorf("repo %q is not in this snapshot; its repositories are %s", args.Repo, strings.Join(labels, ", "))
		}
	}
	named := map[string]bool{}
	if args.Repo != "" {
		named[args.Repo] = true
	}
	// A path's repository is its label prefix, longest first so golf-ui/ is not read as golf.
	byLength := append([]string(nil), labels...)
	sort.Slice(byLength, func(i, j int) bool { return len(byLength[i]) > len(byLength[j]) })
	type split struct{ label, rel string }
	splits := make([]split, 0, len(args.Paths))
	for _, p := range args.Paths {
		p = filepath.ToSlash(s.normalizeToRelative(p))
		sp := split{rel: p}
		for _, l := range byLength {
			if strings.HasPrefix(p, l+"/") {
				sp = split{label: l, rel: strings.TrimPrefix(p, l+"/")}
				break
			}
		}
		if sp.label == "" && args.Repo == "" {
			return planScope{}, fmt.Errorf("path %q names no repository: prefix it with its repo label (%s) or pass repo", p, strings.Join(labels, ", "))
		}
		if sp.label != "" {
			named[sp.label] = true
		}
		splits = append(splits, sp)
	}
	// A fact name selects its repository only when repo does not: the same name can be
	// declared in several members, and an explicit repo is the caller settling that.
	if args.Repo == "" {
		for _, sym := range args.Symbols {
			for _, f := range store.LookupByExactName(sym) {
				if f.Repo != "" {
					named[f.Repo] = true
				}
			}
		}
	}
	if len(named) == 0 {
		return planScope{}, fmt.Errorf("cannot tell which repository this plan is for: pass repo (%s)", strings.Join(labels, ", "))
	}
	if len(named) > 1 {
		got := make([]string, 0, len(named))
		for l := range named {
			got = append(got, l)
		}
		sort.Strings(got)
		return planScope{}, fmt.Errorf("the targets are in %s; a plan covers one repository's declarations, so plan each separately", strings.Join(got, " and "))
	}
	var label string
	for l := range named {
		label = l
	}
	paths := make([]string, 0, len(splits))
	for _, sp := range splits {
		paths = append(paths, sp.rel)
	}
	return planScope{repoPath: repos[label], label: label, paths: paths, multi: true}, nil
}

// governingIntentArgs are the arguments for the governing_intent tool.
type governingIntentArgs struct {
	Target    string `json:"target" jsonschema:"An exact fact name, a file path (repo-relative or label-prefixed), or a compiled page path."`
	Repo      string `json:"repo,omitempty" jsonschema:"Repo label, when the file path is measured in more than one repo."`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"Approximate token cap; output is truncated with a notice. Default: no cap."`
}

// constraintsForArgs are the arguments for the constraints_for tool.
type constraintsForArgs struct {
	Target    string `json:"target" jsonschema:"A repo-relative file path (it may not exist yet) or an exact fact name."`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"Approximate token cap; output is truncated with a notice. Default: no cap."`
}

type planCheckArgs struct {
	Paths     []string `json:"paths,omitempty" jsonschema:"Repo-relative file paths the change touches; they may not exist yet."`
	Symbols   []string `json:"symbols,omitempty" jsonschema:"Exact fact names the change touches."`
	Patch     string   `json:"patch,omitempty" jsonschema:"A unified diff to evaluate on a scratch copy; the working tree is never touched."`
	Repo      string   `json:"repo,omitempty" jsonschema:"Repo label, in a multi-repo snapshot. Inferred from label-prefixed paths and fact names when omitted; needed for a patch, whose paths are relative to one repository."`
	MaxTokens int      `json:"max_tokens,omitempty" jsonschema:"Approximate token cap; output is truncated with a notice. Default: no cap."`
}

// constraintsForResponse is the constraints_for tool's JSON shape: the
// components containing the target, each with the rules binding it, plus the
// current constraint violations whose evidence names the target.
type constraintsForResponse struct {
	Target     string                         `json:"target"`
	Components []constraints.ComponentBinding `json:"components"`
	Violations []facts.Insight                `json:"violations,omitempty"`
}

// renderImpactCompact renders an impact analysis as a compact markdown summary.
// writeGoverningIntent renders the knowledge pages anchored to the target's
// file — the decision trail governing the code whose blast radius is being
// analyzed. Silence when nothing anchors.
func writeGoverningIntent(sb *strings.Builder, pages []facts.GoverningPage) {
	if len(pages) == 0 {
		return
	}
	sb.WriteString("## Governing intent\n\nKnowledge pages anchored to the target's file:\n\n")
	for _, p := range pages {
		sb.WriteString("- " + p.Page + pageMeta(p.Type, p.Status) + "\n")
		for _, r := range p.Relations {
			sb.WriteString("  - " + r.Rel + " " + r.To + pageMeta(r.ToType, r.ToStatus) + "\n")
		}
	}
	sb.WriteString("\n")
}

// pageMeta renders a page's type/status suffix, empty when neither is set.
func pageMeta(pageType, status string) string {
	var meta []string
	if pageType != "" {
		meta = append(meta, pageType)
	}
	if status != "" {
		meta = append(meta, status)
	}
	if len(meta) == 0 {
		return ""
	}
	return " (" + strings.Join(meta, ", ") + ")"
}

// renderGovernedCode renders the forward half of the reverse query: the
// anchors a page declares and the measured coverage under each — a zero is
// visible rather than silent, since an anchor nothing measures is exactly
// what the caller is looking for.
func renderGovernedCode(page string, cov []facts.AnchorCoverage) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "### %s\n\n", page)
	if len(cov) == 0 {
		sb.WriteString("The page compiles but declares no anchors — it governs no code directly.\n")
		return sb.String()
	}
	sb.WriteString("Declared anchors and measured coverage:\n\n")
	for _, c := range cov {
		fmt.Fprintf(&sb, "- %s %s — %d measured fact(s) in %d file(s)\n", c.Repo, c.Path, c.MeasuredFacts, c.MeasuredFiles)
	}
	return sb.String()
}
