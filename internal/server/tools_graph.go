package server

// The graph walk tools: traverse, find_path, endpoint_impact and impact_analysis.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/impact"
	httpsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/http"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTraverseTool registers the traverse tool.
func (s *Server) registerTraverseTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "traverse",
		Description: "Multi-hop walk of the dependency and call graph from one node: forward answers \"what does X depend on?\", reverse answers \"what depends on X?\". Use it instead of repeated explore calls. " +
			"From a struct or interface, forward follows its methods and their calls; reverse also starts from its methods and constructor, so callers that reach the type only through a method (cross-repo too) appear, as in impact_analysis. " +
			"Interface method calls cannot be bound to an implementation statically, so those edges may be missing or show as unresolved nodes. " +
			"Output grows fast: start with output_mode=summary and keep max_depth and max_nodes bounded.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args traverseArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		graph := store.Graph()
		if graph == nil {
			return errorResult("No graph available. Run generate_snapshot first."), nil, nil
		}

		if args.Start == "" {
			return errorResult("start is required"), nil, nil
		}

		// Resolve start name: try exact match first, then substring
		startName, res, err := s.resolveNodeName(store, args.Start)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		mode := resolveOutputMode(args.OutputMode, modeSummary)

		// Over threshold: refuse to guess; return resolution with empty results.
		if res != nil && res.Matched == "" {
			resp := traverseResponse{
				Resolution: res,
				TraversalResult: facts.TraversalResult{
					Nodes: []facts.TraversalNode{},
					Edges: []facts.TraversalEdge{},
				},
			}
			if wantsFullOutput(mode) {
				return jsonResultCapped(resp, args.MaxTokens)
			}
			if wantsSummary(mode) {
				return textResult(capTokens(s.renderTraverseSummary(store, resp, args.Start, ""), args.MaxTokens, false)), nil, nil
			}
			return textResult(capTokens(renderTraverseCompact(resp, args.Start, ""), args.MaxTokens, false)), nil, nil
		}

		direction := args.Direction
		if direction == "" {
			direction = "forward"
		}
		if direction != "forward" && direction != "reverse" {
			return errorResult("direction must be 'forward' or 'reverse'"), nil, nil
		}

		// Reverse traversal of a type must seed its methods + constructor (callers
		// reference those, not the bare type), matching impact_analysis — otherwise
		// cross-repo and same-repo dependents are missed. Forward already follows
		// has_method edges from the type, so it needs no rollup.
		//
		// A name that is one method under several owners — an interface's method and
		// its implementations — is walked from all of them: a call on the interface
		// may reach any, and the implementations are where the calls are.
		starts := []string{startName}
		if res != nil && len(res.Family) > 1 {
			starts = res.Family
		}
		var result facts.TraversalResult
		switch {
		case direction == "reverse":
			var seeds []string
			for _, start := range starts {
				seeds = append(seeds, graph.RollupSeeds(start)...)
			}
			result = graph.TraverseFrom(seeds, direction, args.RelationKinds, args.NodeKinds, args.MaxDepth, args.MaxNodes)
		case len(starts) > 1:
			result = graph.TraverseFrom(starts, direction, args.RelationKinds, args.NodeKinds, args.MaxDepth, args.MaxNodes)
		default:
			result = graph.Traverse(startName, direction, args.RelationKinds, args.NodeKinds, args.MaxDepth, args.MaxNodes)
		}

		resp := traverseResponse{Resolution: res, TraversalResult: result}
		if wantsFullOutput(mode) {
			return jsonResultCapped(resp, args.MaxTokens)
		}
		if wantsSummary(mode) {
			return textResult(capTokens(s.renderTraverseSummary(store, resp, startName, direction), args.MaxTokens, false)), nil, nil
		}
		return textResult(capTokens(renderTraverseCompact(resp, startName, direction), args.MaxTokens, false)), nil, nil
	})
}

// registerFindPathTool registers the find_path tool.
func (s *Server) registerFindPathTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "find_path",
		Description: "Shortest path, by hop count, between two nodes: \"how does X reach Y?\", \"what is the call chain from A to B?\". " +
			"An ambiguous endpoint is not an error: the top candidates are tried (for a type, its methods and constructor too) and the first path found is returned, with the ranked candidates. " +
			"With no path, found=false and a note says whether the endpoints were ambiguous (listing the candidates tried) or resolved but unreachable within max_depth. " +
			"To ask how one set of nodes reaches another (the symbols or modules of one part to those of another), pass " +
			"from_nodes and to_nodes instead: exact node names, one search, the shortest path from any of the first to any of the second. " +
			"A source file path that is not itself a node stands for the symbols the file declares, listed in from_files and to_files. " +
			"A name the graph does not hold is listed in unresolved_from and unresolved_to with its nearest match in from_candidates and to_candidates, which is a suggestion and is never searched.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args findPathArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		graph := store.Graph()
		if graph == nil {
			return errorResult("No graph available. Run generate_snapshot first."), nil, nil
		}

		if len(args.FromNodes) > 0 || len(args.ToNodes) > 0 {
			return s.findPathBetweenSets(store, graph, args)
		}

		if args.From == "" || args.To == "" {
			return errorResult("both 'from' and 'to' are required"), nil, nil
		}

		fromName, fromRes, err := s.resolveNodeName(store, args.From)
		if err != nil {
			return errorResult(fmt.Sprintf("from: %v", err)), nil, nil
		}
		toName, toRes, err := s.resolveNodeName(store, args.To)
		if err != nil {
			return errorResult(fmt.Sprintf("to: %v", err)), nil, nil
		}

		// Build ranked candidate lists for each endpoint (most-likely first) and try
		// a path across the combinations rather than silently giving up when a name
		// is ambiguous. This delivers the "give me vague names and I'll find the
		// connection" behavior.
		fromCands := s.pathCandidates(store, args.From, fromName, fromRes)
		toCands := s.pathCandidates(store, args.To, toName, toRes)
		if len(fromCands) == 0 || len(toCands) == 0 {
			return jsonResult(findPathResponse{
				FromResolution: fromRes,
				ToResolution:   toRes,
				PathResult:     facts.PathResult{From: fromName, To: toName, Found: false},
				Note:           "could not resolve both endpoints to a graph node",
				FromTried:      fromCands,
				ToTried:        toCands,
			})
		}

		result := s.bestPath(graph, fromCands, toCands, args.RelationKinds, args.MaxDepth)

		resp := findPathResponse{
			FromResolution: fromRes,
			ToResolution:   toRes,
			PathResult:     result,
			FromTried:      fromCands,
			ToTried:        toCands,
		}
		if !result.Found {
			ambiguous := len(fromCands) > 1 || len(toCands) > 1
			if ambiguous {
				resp.Note = fmt.Sprintf("no path within %d hops between any candidate pair "+
					"(from: %d candidate(s), to: %d candidate(s)). Narrow with a package-qualified "+
					"name (e.g. \"repo:<label> pkg.Type\") — see from_tried/to_tried.",
					effectiveMaxDepth(args.MaxDepth), len(fromCands), len(toCands))
			} else {
				resp.Note = fmt.Sprintf("both endpoints resolved uniquely, but no path connects them within %d hops",
					effectiveMaxDepth(args.MaxDepth))
			}
		}
		return jsonResult(resp)
	})
}

// findPathSetsResponse answers find_path over from_nodes and to_nodes: the
// shortest path between the two sets and every name the graph does not hold.
// It carries no resolution, because nothing was resolved.
type findPathSetsResponse struct {
	Note string `json:"note,omitempty"`
	// FromFiles and ToFiles name each source file a set expanded into the
	// symbols it declares, with how many; FromCandidates and ToCandidates map
	// an unresolved name to its nearest match.
	FromFiles      map[string]int    `json:"from_files,omitempty"`
	ToFiles        map[string]int    `json:"to_files,omitempty"`
	FromCandidates map[string]string `json:"from_candidates,omitempty"`
	ToCandidates   map[string]string `json:"to_candidates,omitempty"`
	facts.SetPathResult
}

// maxSetSuggestions bounds how many unresolved names of one side get a nearest
// match, each of which is a ranked search of the facts.
const maxSetSuggestions = 20

// findPathBetweenSets answers the set form of find_path. The sets replace from
// and to rather than mixing with them, so a call reads as one question: an
// endpoint that is a single name is a set of one.
func (s *Server) findPathBetweenSets(store *facts.Store, graph *facts.Graph, args findPathArgs) (*mcp.CallToolResult, any, error) {
	if args.From != "" || args.To != "" {
		return errorResult("from_nodes and to_nodes replace from and to; pass one pair or the other"), nil, nil
	}
	if len(args.FromNodes) == 0 || len(args.ToNodes) == 0 {
		return errorResult("both 'from_nodes' and 'to_nodes' are required when either is given"), nil, nil
	}
	froms, fromFiles := s.expandDeclaredFiles(store, args.FromNodes)
	tos, toFiles := s.expandDeclaredFiles(store, args.ToNodes)
	result := graph.FindPathBetween(froms, tos, args.RelationKinds, args.MaxDepth)
	resp := findPathSetsResponse{
		SetPathResult:  result,
		FromFiles:      fromFiles,
		ToFiles:        toFiles,
		FromCandidates: s.nearestNames(store, result.UnresolvedFrom),
		ToCandidates:   s.nearestNames(store, result.UnresolvedTo),
	}
	if !result.Found {
		resp.Note = fmt.Sprintf("no path within %d hops from any of %d source(s) to any of %d target(s)",
			effectiveMaxDepth(args.MaxDepth), len(froms)-len(result.UnresolvedFrom), len(tos)-len(result.UnresolvedTo))
		if len(result.UnresolvedFrom)+len(result.UnresolvedTo) > 0 {
			resp.Note += "; names the graph does not hold are listed in unresolved_from and unresolved_to"
		}
	}
	return jsonResult(resp)
}

// expandDeclaredFiles replaces each name that is a source file, and no node of
// the graph, with the symbols the file declares, since a file such as a Rails
// controller is never a node itself and its symbols are. It returns the names
// to search and, for each file expanded, how many symbols it stood for. A file
// that more than one repository holds under that path is left as it is, so the
// call names the repository or stays unresolved: guessing which was meant would
// put an edge in the answer that the caller did not ask about.
func (s *Server) expandDeclaredFiles(store *facts.Store, names []string) ([]string, map[string]int) {
	r := s.resolver(store)
	out := make([]string, 0, len(names))
	var expanded map[string]int
	for _, name := range names {
		if len(store.LookupByExactName(name)) > 0 {
			out = append(out, name)
			continue
		}
		rel := r.NormalizeToRelative(name)
		var declared []string
		holders := 0
		forms := slices.Compact(append([]string{rel}, r.ExpandFilePrefix(rel)...))
		for _, file := range forms {
			var here []string
			for _, f := range store.ByFile(file) {
				if f.Kind == facts.KindSymbol {
					here = append(here, f.Name)
				}
			}
			if len(here) > 0 {
				holders++
				declared = here
			}
		}
		if holders != 1 {
			out = append(out, name)
			continue
		}
		slices.Sort(declared)
		declared = slices.Compact(declared)
		out = append(out, declared...)
		if expanded == nil {
			expanded = map[string]int{}
		}
		expanded[name] = len(declared)
	}
	return out, expanded
}

// nearestNames maps each of the first maxSetSuggestions unresolved names to the
// fact name the single-pair form would try first for it. It only suggests: a
// name is never searched under a guess.
func (s *Server) nearestNames(store *facts.Store, unresolved []string) map[string]string {
	var out map[string]string
	for i, name := range unresolved {
		if i == maxSetSuggestions {
			break
		}
		resolved, res, err := s.resolveNodeName(store, name)
		if err != nil {
			continue
		}
		tried := s.pathCandidates(store, name, resolved, res)
		if len(tried) == 0 {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[name] = tried[0]
	}
	return out
}

// registerEndpointImpactTool registers the endpoint_impact tool.
func (s *Server) registerEndpointImpactTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "endpoint_impact",
		Description: "What changing an HTTP endpoint reaches: the route, the controller serving it, the models that controller touches, the models associated with those, and the tables behind them. " +
			"Use this when you have a URL, impact_analysis when you have a symbol. " +
			"It answers about what the application serves: client call sites and mock-server routes are excluded.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args endpointImpactArgs) (*mcp.CallToolResult, any, error) {
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		if args.Endpoint == "" {
			return errorResult("endpoint is required"), nil, nil
		}
		// Callers come from the cross-repo linker's own matching, under the server's
		// linking vocabulary, so a call it linked is a caller here too.
		linkVocab, err := s.eng.Config().LinkingVocab()
		if err != nil {
			return errorResult(fmt.Sprintf("invalid linking vocabulary: %v", err)), nil, nil
		}
		return jsonResult(httpsignal.AnalyzeEndpoint(store, linkVocab, args.Endpoint, args.MaxRoutes))
	})
}

// registerImpactAnalysisTool registers the impact_analysis tool.
func (s *Server) registerImpactAnalysisTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "impact_analysis",
		Description: "Blast radius of changing a node: everything that transitively depends on it, grouped by hop depth. For refactoring plans and change risk. " +
			"include_forward=true adds what the target itself depends on. " +
			"When knowledge pages declare anchors (enola_intent), it also lists the pages anchored to the target's file: the decisions governing the code you are about to change. " +
			"Output grows fast: start with output_mode=summary and keep max_depth and max_nodes bounded.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args impactAnalysisArgs) (*mcp.CallToolResult, any, error) {
		report, err := impact.Analyze(s.resolver(s.eng.Store()), impact.Request{
			Target:         args.Target,
			MaxDepth:       args.MaxDepth,
			MaxNodes:       args.MaxNodes,
			IncludeForward: args.IncludeForward,
		})
		switch {
		case errors.Is(err, impact.ErrNoFacts):
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		case errors.Is(err, impact.ErrNoGraph):
			return errorResult("No graph available. Run generate_snapshot first."), nil, nil
		case err != nil:
			return errorResult(err.Error()), nil, nil
		}
		mode := resolveOutputMode(args.OutputMode, modeSummary)
		if wantsFullOutput(mode) {
			return jsonResultCapped(report, args.MaxTokens)
		}
		if wantsSummary(mode) {
			return textResult(capTokens(s.renderImpactSummary(report), args.MaxTokens, false)), nil, nil
		}
		return textResult(capTokens(renderImpactCompact(report), args.MaxTokens, false)), nil, nil
	})
}

// maxPathCandidates caps how many ranked candidates find_path considers per
// endpoint when a name is ambiguous.
const maxPathCandidates = 8

// maxPathAttempts caps the total FindPath probes find_path runs across the
// candidate/seed combinations, so a doubly-ambiguous query stays bounded.
const maxPathAttempts = 40

// pathCandidates returns the ranked node names find_path should try for one
// endpoint, most-likely first and capped at maxPathCandidates. It leads with the
// resolved name (preserving resolveNodeName's exact/service/file-basename
// fallbacks), then any candidates from the resolution, then broadens with a fresh
// ranking so a heavily-ambiguous name (10+ matches) is not limited to the few
// candidates echoed in the resolution object.
func (s *Server) pathCandidates(store *facts.Store, input, resolved string, res *nameResolution) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, maxPathCandidates)
	add := func(n string) {
		if n == "" || len(out) >= maxPathCandidates {
			return
		}
		if _, dup := seen[n]; dup {
			return
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	add(resolved)
	if res != nil {
		for _, c := range res.Candidates {
			add(c.Name)
		}
	}
	for _, c := range s.rankedCandidatesFor(store, input) {
		add(c.Name)
	}
	return out
}

// firstOr returns the first element of s, or fallback when s is empty.
func firstOr(s []string, fallback string) string {
	if len(s) == 0 {
		return fallback
	}
	return s[0]
}

// bestPath tries to connect any from-candidate to any to-candidate, expanding each
// to-candidate with RollupSeeds (a path to a type usually ends at one of its
// methods/constructor). It scans in ranked order, keeps the shortest path found,
// and is bounded by maxPathAttempts (returning early on a ≤2-hop hit). When no
// path exists it returns a not-found PathResult naming the first from/to tried.
func (s *Server) bestPath(graph *facts.Graph, fromCands, toCands []string, relKinds []string, maxDepth int) facts.PathResult {
	// Self-defensive: callers are expected to pass non-empty candidate slices, but
	// guard so an empty slice yields a clean not-found result instead of indexing
	// fromCands[0]/toCands[0] out of range.
	if len(fromCands) == 0 || len(toCands) == 0 {
		return facts.PathResult{From: firstOr(fromCands, ""), To: firstOr(toCands, ""), Found: false}
	}
	var best facts.PathResult
	attempts := 0
	for _, from := range fromCands {
		for _, to := range toCands {
			for _, target := range graph.RollupSeeds(to) {
				if attempts >= maxPathAttempts {
					if best.Found {
						return best
					}
					return facts.PathResult{From: fromCands[0], To: toCands[0], Found: false}
				}
				attempts++
				r := graph.FindPath(from, target, relKinds, maxDepth)
				if !r.Found {
					continue
				}
				if !best.Found || len(r.Path) < len(best.Path) {
					best = r
				}
				if len(best.Path) <= 2 { // direct or one-hop: good enough
					return best
				}
			}
		}
	}
	if best.Found {
		return best
	}
	return facts.PathResult{From: fromCands[0], To: toCands[0], Found: false}
}

// effectiveMaxDepth mirrors graph.FindPath's clamping (0 → 10, cap 20) so the
// not-found note reports the depth actually searched.
func effectiveMaxDepth(maxDepth int) int {
	if maxDepth <= 0 {
		return 10
	}
	if maxDepth > 20 {
		return 20
	}
	return maxDepth
}

// traverseArgs are the arguments for the traverse tool.
type traverseArgs struct {
	Start         string   `json:"start" jsonschema:"Starting node (fact, module or symbol name), substring match. Disambiguate with repo:, kind:, file: prefixes or a package-qualified name, e.g. 'repo:go-auth kind:struct AuthHandler' or 'domain/cart.CartService'. An ambiguous name returns ranked candidates with confidence."`
	Direction     string   `json:"direction,omitempty" jsonschema:"'forward' (default) follows outgoing relations, 'reverse' incoming ones."`
	RelationKinds []string `json:"relation_kinds,omitempty" jsonschema:"Relations to follow: imports, calls, declares, implements, depends_on, has_method. Default: all."`
	MaxDepth      int      `json:"max_depth,omitempty" jsonschema:"Maximum depth (1-20). Default 5."`
	MaxNodes      int      `json:"max_nodes,omitempty" jsonschema:"Maximum nodes (1-500); the walk stops there. Default 100."`
	NodeKinds     []string `json:"node_kinds,omitempty" jsonschema:"Filters the output, not the walk: module, symbol, dependency, route, storage. Default: all."`
	OutputMode    string   `json:"output_mode,omitempty" jsonschema:"'summary' (DEFAULT, counts by node and relation kind, internal/external split, hottest modules), 'compact' (nodes grouped by depth), or 'full' (raw JSON node/edge graph; can be VERY large). Escalate only when you need specific nodes."`
	MaxTokens     int      `json:"max_tokens,omitempty" jsonschema:"Approximate token cap; output is truncated with a notice. Default: no cap."`
}

// findPathArgs are the arguments for the find_path tool.
type findPathArgs struct {
	From          string   `json:"from,omitempty" jsonschema:"Source node, substring match, required unless from_nodes is given. Disambiguate with repo:, kind:, file: prefixes or a package-qualified name, e.g. 'repo:go-auth Login' or 'ticket.Repository'."`
	To            string   `json:"to,omitempty" jsonschema:"Target node, matched like from, required unless to_nodes is given, e.g. 'kind:struct AuthMiddleware' or 'repo:golf domain/cart.CartService'."`
	RelationKinds []string `json:"relation_kinds,omitempty" jsonschema:"Relations to follow. Default: all."`
	MaxDepth      int      `json:"max_depth,omitempty" jsonschema:"Maximum path length (1-20). Default 10."`
	FromNodes     []string `json:"from_nodes,omitempty" jsonschema:"Exact node names to start from, used together with to_nodes instead of from and to: one breadth-first search finds the shortest path from any of them to any of to_nodes. No substring matching; a name the graph does not hold is listed in unresolved_from."`
	ToNodes       []string `json:"to_nodes,omitempty" jsonschema:"Exact node names to reach, used together with from_nodes instead of from and to. Each is expanded to a type's methods and constructor as to is. No substring matching; a name the graph does not hold is listed in unresolved_to."`
}

// impactAnalysisArgs are the arguments for the impact_analysis tool.
type endpointImpactArgs struct {
	Endpoint  string `json:"endpoint" jsonschema:"Path substring, optionally prefixed with a verb: 'GET /v1/candidates' or '/v1/candidates'."`
	MaxRoutes int    `json:"max_routes,omitempty" jsonschema:"Matched endpoints to follow (1-200); a bare prefix can match hundreds. Default 25."`
}

type impactAnalysisArgs struct {
	Target         string `json:"target" jsonschema:"The node being changed, substring match. Disambiguate with repo:, kind:, file: prefixes."`
	MaxDepth       int    `json:"max_depth,omitempty" jsonschema:"Hops of impact (1-10). Default 3."`
	MaxNodes       int    `json:"max_nodes,omitempty" jsonschema:"Maximum impacted nodes (1-500). Default 200."`
	IncludeForward bool   `json:"include_forward,omitempty" jsonschema:"Also show what the target depends on, i.e. what could break it. Default false."`
	OutputMode     string `json:"output_mode,omitempty" jsonschema:"'summary' (DEFAULT, exact dependent count, breakdown by kind and depth, hotspot modules, cross-repo reach, cycle and layer insights touching the target), 'compact' (dependents by depth), or 'full' (raw JSON graph; can be VERY large). Escalate only when you need specific nodes."`
	MaxTokens      int    `json:"max_tokens,omitempty" jsonschema:"Approximate token cap; output is truncated with a notice. Default: no cap."`
}

// compactPerDepthCap bounds how many nodes are listed per depth bucket in the
// compact graph-tool output, keeping responses well under the MCP token budget
// even for high-fan-in nodes. The accurate totals still reflect the full set.
const compactPerDepthCap = 40

// writeResolutionNote renders the name-resolution context (if any) for the
// compact graph-tool output: when a name was ambiguous and refused, it lists the
// candidates so the caller can re-run with an exact name; when it was auto-picked
// it notes the match and alternatives.
// writeImpactResolution is writeResolutionNote for an impact report, which may be
// about several nodes at once (a source file, a namespace, a directory): the note
// says what was walked back from instead.
func writeImpactResolution(sb *strings.Builder, resp impactResponse) {
	n := len(resp.Seeds)
	switch {
	case n > 0 && resp.SeedKind == impact.SeedsNamespace:
		writeResolutionNote(sb, resp.Resolution)
		fmt.Fprintf(sb, "> %s is a namespace: this is the impact of it and the %d symbols nested under it.\n\n", resp.Target, n)
	case n > 0 && resp.SeedKind == impact.SeedsFamily:
		writeResolutionNote(sb, resp.Resolution)
	case n > 0 && resp.SeedKind == impact.SeedsPackage:
		writeResolutionNote(sb, resp.Resolution)
		fmt.Fprintf(sb, "> %s is a package: its %d own symbols are walked from as part of it, not listed as its dependents.\n\n", resp.Target, n)
	case n > 0 && resp.SeedKind == impact.SeedsDirectory:
		if resp.Resolution != nil && resp.Resolution.Matched != resp.Target {
			writeResolutionNote(sb, resp.Resolution)
		}
		fmt.Fprintf(sb, "> %s is a directory: this is the impact of the %d symbols declared beneath it (its own tests excluded).\n\n", resp.Target, n)
	case n > 0:
		fmt.Fprintf(sb, "> %s is a file, not a node: this is the impact of the %d symbols it declares.\n\n", resp.Resolution.Matched, n)
	default:
		writeResolutionNote(sb, resp.Resolution)
	}
}

// maxImpactPackages caps the per-package dependent counts an impact summary lists.
const maxImpactPackages = 15

// maxImpactModulesPerDepth caps how many dependent modules one hop lists.
const maxImpactModulesPerDepth = 60

// writeImpactModules lists the modules that depend on a module target, by hop. It
// comes first: for a package, which packages depend on it is the answer, and the
// symbol walk below it is the detail.
func writeImpactModules(sb *strings.Builder, resp impactResponse) {
	if len(resp.Modules) == 0 {
		return
	}
	byDepth := map[int][]string{}
	var depths []int
	for _, m := range resp.Modules {
		if _, ok := byDepth[m.Depth]; !ok {
			depths = append(depths, m.Depth)
		}
		name := m.Module
		if m.Repo != "" {
			name = m.Repo + ":" + name
		}
		byDepth[m.Depth] = append(byDepth[m.Depth], name)
	}
	sort.Ints(depths)
	fmt.Fprintf(sb, "## Dependent packages (%d; %d direct)\n\n", len(resp.Modules), len(byDepth[1]))
	for _, d := range depths {
		names := byDepth[d]
		shown := names
		if len(shown) > maxImpactModulesPerDepth {
			shown = shown[:maxImpactModulesPerDepth]
		}
		fmt.Fprintf(sb, "- %d hop%s: %s", d, map[bool]string{true: "", false: "s"}[d == 1], strings.Join(shown, ", "))
		if len(names) > len(shown) {
			fmt.Fprintf(sb, ", … %d more", len(names)-len(shown))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
}

// writeImpactPackages lists the listed dependents per owning package, when the
// repository declares packages.
func writeImpactPackages(sb *strings.Builder, resp impactResponse) {
	if len(resp.Packages) == 0 {
		return
	}
	sb.WriteString("## Dependents by package\n\n")
	for i, p := range resp.Packages {
		if i == maxImpactPackages {
			fmt.Fprintf(sb, "- … %d more packages\n", len(resp.Packages)-maxImpactPackages)
			break
		}
		fmt.Fprintf(sb, "- %s (%d dependents)\n", p.Package, p.Dependents)
	}
	if resp.Stats.Truncated {
		sb.WriteString("\nCounts cover the listed dependents only; raise max_nodes for all of them.\n")
	}
	sb.WriteString("\n")
}

func writeResolutionNote(sb *strings.Builder, res *nameResolution) {
	if res == nil {
		return
	}
	if res.Matched == "" {
		fmt.Fprintf(sb, "> ⚠ Ambiguous %q — refine with an exact name or a repo:/kind:/file: scope. Candidates:\n", res.Query)
		writeCandidateList(sb, res)
		sb.WriteString("\n")
		return
	}
	if len(res.Family) > 1 {
		fmt.Fprintf(sb, "> %q names %d methods of the same name; walked all of them:\n", res.Query, len(res.Family))
		for _, name := range res.Family {
			sb.WriteString("> - " + name + "\n")
		}
		sb.WriteString("\n")
		return
	}
	fmt.Fprintf(sb, "> Resolved %q → %s", res.Query, res.Matched)
	if res.Confidence > 0 {
		fmt.Fprintf(sb, " (confidence %.2f)", res.Confidence)
	}
	sb.WriteString("\n")
	if len(res.Alternatives) > 0 {
		fmt.Fprintf(sb, "> alternatives: %s\n", strings.Join(res.Alternatives, ", "))
	}
	sb.WriteString("\n")
}

// writeCandidateList prints a resolution's ranked candidates (falling back to the
// plain alternative names when no scored candidates are present).
func writeCandidateList(sb *strings.Builder, res *nameResolution) {
	if len(res.Candidates) > 0 {
		listed := map[string]bool{}
		for _, c := range res.Candidates {
			listed[c.Name] = true
			line := "> - " + c.Name
			if c.Kind != "" {
				line += " (" + c.Kind + ")"
			}
			if c.File != "" {
				line += " — " + c.File
			}
			sb.WriteString(line + "\n")
		}
		// The scored candidates are the top three; the rest are named too, so a
		// caller does not take three for all there are.
		var more []string
		for _, a := range res.Alternatives {
			if !listed[a] {
				more = append(more, a)
			}
		}
		if len(more) > 0 {
			sb.WriteString("> - also: " + strings.Join(more, ", ") + "\n")
		}
		if shown := len(res.Candidates) + len(more); res.Total > shown {
			fmt.Fprintf(sb, "> (%d names match; %d shown)\n", res.Total, shown)
		}
		return
	}
	for _, a := range res.Alternatives {
		sb.WriteString("> - " + a + "\n")
	}
}

// otherKinds returns the conflated kinds except the one already displayed, so the
// annotation adds information instead of repeating the node's own kind.
func otherKinds(conflated []string, shown string) []string {
	out := make([]string, 0, len(conflated))
	for _, k := range conflated {
		if k != shown {
			out = append(out, k)
		}
	}
	return out
}

// compactNodeLine formats a single traversal node as a markdown list item.
func compactNodeLine(n facts.TraversalNode) string {
	s := "- " + n.Name
	if n.Kind != "" {
		s += " (" + n.Kind + ")"
	}
	if n.Unresolved {
		s += " [unresolved]"
	}
	if len(n.Conflated) > 1 {
		// The node's name is shared by facts of several kinds, so it merges them.
		// Say so rather than letting the single kind above read as precise.
		s += " [also: " + strings.Join(otherKinds(n.Conflated, n.Kind), ", ") + "]"
	}
	if n.File != "" {
		s += " — " + n.File
		if n.Line > 0 {
			s += fmt.Sprintf(":%d", n.Line)
		}
	}
	return s
}

// writeNodesByDepth groups nodes by their depth (skipping depth 0, the origin)
// and writes one capped section per depth.
func writeNodesByDepth(sb *strings.Builder, nodes []facts.TraversalNode) {
	byDepth := map[int][]facts.TraversalNode{}
	for _, n := range nodes {
		if n.Depth > 0 {
			byDepth[n.Depth] = append(byDepth[n.Depth], n)
		}
	}
	depths := make([]int, 0, len(byDepth))
	for d := range byDepth {
		depths = append(depths, d)
	}
	sort.Ints(depths)
	for _, d := range depths {
		group := byDepth[d]
		fmt.Fprintf(sb, "## Depth %d (%d)\n\n", d, len(group))
		shown := group
		if len(shown) > compactPerDepthCap {
			shown = shown[:compactPerDepthCap]
		}
		for _, n := range shown {
			sb.WriteString(compactNodeLine(n) + "\n")
		}
		if len(group) > compactPerDepthCap {
			fmt.Fprintf(sb, "... and %d more at depth %d\n", len(group)-compactPerDepthCap, d)
		}
		sb.WriteString("\n")
	}
}

func renderImpactCompact(resp impactResponse) string {
	var sb strings.Builder
	r := resp.ImpactResult
	fmt.Fprintf(&sb, "# Impact: %s\n\n", r.Target)
	writeImpactResolution(&sb, resp)

	// Ambiguous-and-refused: no traversal was run; the candidate list above is
	// the actionable content.
	if resp.Resolution != nil && resp.Resolution.Matched == "" {
		return sb.String()
	}

	writeImpactModules(&sb, resp)
	if r.Summary != "" {
		sb.WriteString(r.Summary + "\n\n")
	} else {
		sb.WriteString("No dependents found.\n\n")
	}

	writeImpactPackages(&sb, resp)
	writeGoverningIntent(&sb, r.GoverningIntent)

	// Flatten ByDepth into a node slice for grouped rendering.
	var nodes []facts.TraversalNode
	for _, group := range r.ByDepth {
		nodes = append(nodes, group...)
	}
	writeNodesByDepth(&sb, nodes)

	if r.Forward != nil && len(r.Forward.Nodes) > 0 {
		fmt.Fprintf(&sb, "# Forward dependencies of %s (what it depends on)\n\n", r.Target)
		writeNodesByDepth(&sb, r.Forward.Nodes)
	}

	fmt.Fprintf(&sb, "_Stats: %d visited, max depth %d", r.Stats.NodesVisited, r.Stats.MaxDepthReached)
	if r.Stats.Truncated {
		sb.WriteString(", truncated — use max_nodes to widen or output_mode=full for the raw graph")
	}
	sb.WriteString("._\n")
	return sb.String()
}

// renderTraverseCompact renders a graph traversal as a compact markdown summary.
func renderTraverseCompact(resp traverseResponse, start, direction string) string {
	var sb strings.Builder
	if direction == "" {
		direction = "forward"
	}
	fmt.Fprintf(&sb, "# Traverse: %s (%s)\n\n", start, direction)
	writeResolutionNote(&sb, resp.Resolution)

	if resp.Resolution != nil && resp.Resolution.Matched == "" {
		return sb.String()
	}

	// Node count excludes depth-0 origin(s).
	reached := 0
	for _, n := range resp.Nodes {
		if n.Depth > 0 {
			reached++
		}
	}
	fmt.Fprintf(&sb, "Reached **%d** nodes", reached)
	if resp.Stats.Truncated {
		sb.WriteString(" (truncated — more exist beyond the cap)")
	}
	sb.WriteString("\n\n")

	writeNodesByDepth(&sb, resp.Nodes)

	fmt.Fprintf(&sb, "_Edges: %d (use output_mode=full for the raw node/edge graph). Stats: %d visited, max depth %d._\n",
		len(resp.Edges), resp.Stats.NodesVisited, resp.Stats.MaxDepthReached)
	return sb.String()
}

// renderTraverseSummary renders a graph traversal as an aggregated summary:
// counts by node kind and edge relation kind, internal/external split, and the
// hottest target modules — no per-node list. This is the default, token-cheapest
// view; escalate to compact/full for node-level detail.
func (s *Server) renderTraverseSummary(store *facts.Store, resp traverseResponse, start, direction string) string {
	var sb strings.Builder
	if direction == "" {
		direction = "forward"
	}
	fmt.Fprintf(&sb, "# Traverse summary: %s (%s)\n\n", start, direction)
	writeResolutionNote(&sb, resp.Resolution)
	if resp.Resolution != nil && resp.Resolution.Matched == "" {
		return sb.String()
	}

	byKind := map[string]int{}
	byModule := map[string]int{}
	reached := 0
	for _, n := range resp.Nodes {
		if n.Depth == 0 {
			continue // skip origin
		}
		reached++
		byKind[n.Kind]++
		byModule[moduleOf(n)]++
	}

	byRel := map[string]int{}
	for _, e := range resp.Edges {
		byRel[e.Kind]++
	}

	verb := "depends on"
	if direction == "reverse" {
		verb = "is used by"
	}
	fmt.Fprintf(&sb, "%s **%s** %d nodes across %d modules", start, verb, reached, len(byModule))
	if resp.Stats.Truncated {
		sb.WriteString(" (truncated — more exist beyond the cap; raise max_nodes or narrow relation_kinds)")
	}
	sb.WriteString("\n\n")

	if len(byKind) > 0 {
		sb.WriteString("## By node kind\n\n")
		for _, k := range topCounts(byKind, len(byKind)) {
			fmt.Fprintf(&sb, "- %s: %d\n", k, byKind[k])
		}
		sb.WriteString("\n")
	}

	if len(byRel) > 0 {
		total := len(resp.Edges)
		sb.WriteString("## By relation kind\n\n")
		for _, k := range topCounts(byRel, len(byRel)) {
			pct := 0
			if total > 0 {
				pct = byRel[k] * 100 / total
			}
			fmt.Fprintf(&sb, "- %s: %d (%d%%)\n", k, byRel[k], pct)
		}
		sb.WriteString("\n")
	}

	// Internal vs external split for dependency nodes (looked up via the fact store).
	var internal, external, stdlib, framework int
	for _, n := range resp.Nodes {
		if n.Depth == 0 || n.Kind != facts.KindDependency {
			continue
		}
		for _, f := range store.LookupByExactName(n.Name) {
			src, _ := f.PropAny("source").(string)
			switch src {
			case "external":
				external++
			case "stdlib":
				stdlib++
			case "internal":
				internal++
			case "framework":
				framework++
			}
			break
		}
	}
	if internal+external+stdlib+framework > 0 {
		fmt.Fprintf(&sb, "## Dependency sources\n\n- internal: %d\n- external: %d\n- stdlib: %d\n- framework: %d\n\n", internal, external, stdlib, framework)
	}

	if len(byModule) > 0 {
		sb.WriteString("## Hottest modules\n\n")
		for _, m := range topCounts(byModule, 5) {
			fmt.Fprintf(&sb, "- %s (%d nodes)\n", m, byModule[m])
		}
		sb.WriteString("\n")
	}

	// NodesVisited counts every node the walk PASSED THROUGH, including those a
	// node_kinds filter excluded from the result (graph.go traverses through them to
	// reach their neighbours). Printing it as a bare "N nodes" put an unfiltered
	// number beside a filtered list — node_kinds=["route"] could report "used by 2
	// nodes" above "Stats: 431 nodes". Label the two counts for what they are.
	fmt.Fprintf(&sb, "_Stats: %d nodes matched, %d walked, %d edges, max depth %d. Use output_mode=compact for the per-depth node list, output_mode=full for the raw graph._\n",
		reached, resp.Stats.NodesVisited, len(resp.Edges), resp.Stats.MaxDepthReached)
	return sb.String()
}

// renderImpactSummary renders an impact analysis as an aggregated summary:
// total dependents, breakdown by kind and depth, hotspot modules, cross-repo
// reach, and any architectural insights touching the target — no per-node list.
func (s *Server) renderImpactSummary(resp impactResponse) string {
	var sb strings.Builder
	r := resp.ImpactResult
	fmt.Fprintf(&sb, "# Impact summary: %s\n\n", r.Target)
	writeImpactResolution(&sb, resp)
	if resp.Resolution != nil && resp.Resolution.Matched == "" {
		return sb.String()
	}

	writeImpactModules(&sb, resp)
	if r.Summary != "" {
		sb.WriteString(r.Summary + "\n\n")
	}
	fmt.Fprintf(&sb, "**%d** total transitive dependents, the farthest %d %s away.\n\n", r.TotalDependents, r.Stats.MaxDepthReached,
		map[bool]string{true: "hop", false: "hops"}[r.Stats.MaxDepthReached == 1])

	byKind := map[string]int{}
	byModule := map[string]int{}
	depths := make([]int, 0, len(r.ByDepth))
	for d := range r.ByDepth {
		depths = append(depths, d)
	}
	sort.Ints(depths)
	for _, d := range depths {
		for _, n := range r.ByDepth[d] {
			byKind[n.Kind]++
			byModule[moduleOf(n)]++
		}
	}

	if len(byKind) > 0 {
		sb.WriteString("## Dependents by kind\n\n")
		for _, k := range topCounts(byKind, len(byKind)) {
			fmt.Fprintf(&sb, "- %s: %d\n", k, byKind[k])
		}
		sb.WriteString("\n")
	}

	if len(depths) > 0 {
		sb.WriteString("## By depth\n\n")
		for _, d := range depths {
			fmt.Fprintf(&sb, "- depth %d: %d\n", d, len(r.ByDepth[d]))
		}
		sb.WriteString("\n")
	}

	writeImpactPackages(&sb, resp)

	if len(byModule) > 0 {
		sb.WriteString("## Hotspot modules\n\n")
		for _, m := range topCounts(byModule, 5) {
			fmt.Fprintf(&sb, "- %s (%d dependents)\n", m, byModule[m])
		}
		sb.WriteString("\n")
	}

	if len(r.CrossRepoImpact) > 0 {
		fmt.Fprintf(&sb, "## Cross-repo impact\n\nOther repos with a dependent: %s\n\n", strings.Join(r.CrossRepoImpact, ", "))
	}

	writeGoverningIntent(&sb, r.GoverningIntent)

	if r.Forward != nil && len(r.Forward.Nodes) > 0 {
		fwd := map[string]int{}
		for _, n := range r.Forward.Nodes {
			if n.Depth > 0 {
				fwd[n.Kind]++
			}
		}
		sb.WriteString("## Forward dependencies (what the target depends on)\n\n")
		for _, k := range topCounts(fwd, len(fwd)) {
			fmt.Fprintf(&sb, "- %s: %d\n", k, fwd[k])
		}
		sb.WriteString("\n")
	}

	writeInsightList(&sb, "Architectural insights touching this target", s.insightsFor(r.Target))

	sb.WriteString("_Use output_mode=compact for the per-depth dependent list, output_mode=full for the raw graph._\n")
	return sb.String()
}

// traverseResponse wraps a graph traversal with the optional name resolution.
// The embedded TraversalResult inlines nodes/edges/stats alongside resolution.
type traverseResponse struct {
	Resolution *nameResolution `json:"resolution,omitempty"`
	facts.TraversalResult
}

// impactResponse is impact.Report under the server's name for it.
type impactResponse = impact.Report

// findPathResponse wraps a shortest-path result. find_path resolves two names,
// so it carries one resolution per side.
type findPathResponse struct {
	FromResolution *nameResolution `json:"from_resolution,omitempty"`
	ToResolution   *nameResolution `json:"to_resolution,omitempty"`
	// Note explains a found:false result — whether the endpoints were ambiguous
	// (with the candidates tried) or resolved uniquely but unreachable.
	Note string `json:"note,omitempty"`
	// FromTried/ToTried are the ranked endpoint candidates find_path attempted,
	// most-likely first, so the caller can see what was searched.
	FromTried []string `json:"from_tried,omitempty"`
	ToTried   []string `json:"to_tried,omitempty"`
	facts.PathResult
}
