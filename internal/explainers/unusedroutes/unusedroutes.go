// Package unusedroutes provides an explainer that summarizes server routes which
// no loaded client repo calls — endpoints the engine's cross-repo link pass
// flagged with the "unmatched_by_clients" prop — into "unused endpoint" candidate
// insights.
//
// The flag is computed structurally from the loaded snapshot only, so this
// explainer is deliberately careful to frame results as *candidates*: a route is
// listed because no client in the snapshot calls it, which is not the same as
// dead. Consumers outside the snapshot — admin/ops scripts, cron jobs, webhooks,
// third-party API clients, mobile deep links — do not appear here, so each
// candidate must be verified against those before removal.
package unusedroutes

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/providers"
)

// incompleteClientsShare is the proportion of unmatched routes above which the
// finding is about the client set rather than the endpoints.
const incompleteClientsShare = 0.8

// maxActionableCandidates is how many candidates a person can plausibly triage.
// Past it the output stops being a list and becomes a census: a large Rails
// monolith reports 3,392 unmatched at 77% — under the share threshold and still
// unreadable as "endpoints to consider removing". A finding nobody can act on
// is telemetry.
const maxActionableCandidates = 200

// maxSamples bounds how many route names an insight lists inline.
const maxSamples = 25

// Explainer emits one insight per repo whose server routes include endpoints that
// no loaded client calls.
type Explainer struct{}

// New creates a new Explainer.
func New() *Explainer { return &Explainer{} }

func (e *Explainer) Name() string { return "unused-routes" }

// Explain groups client-unmatched server routes by repo and emits one insight per
// repo, each carrying the mandatory out-of-snapshot caveat. It returns nothing for
// single-repo snapshots (no service nodes, so the cross-repo flagging never ran).
func (e *Explainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	// A route registered twice needs no client to be found, so it is reported in
	// a single-repo snapshot as well.
	duplicates := duplicateRoutes(store)
	if len(store.ByKind(facts.KindService)) == 0 {
		return duplicates, nil
	}

	// Served counts every server route per repo, so an unmatched count can be
	// read as a proportion. Without it a finding says "3,392 route(s) have no
	// caller" and reads as 3,392 dead endpoints, when what it actually means is
	// that the client set is incomplete.
	served, declined := map[string]int{}, map[string]int{}
	// The assessed routes per file, so a finding too broad to list can still say
	// where the unmatched ones are.
	servedIn := map[string]map[string]int{}
	for _, f := range store.ByKind(facts.KindRoute) {
		if f.Props == nil {
			continue
		}
		if role, _ := f.PropAny("role").(string); role == "client" {
			continue
		}
		if level, _ := f.PropAny(providers.PropResolutionLevel).(string); level == providers.LevelRuntimeObserved {
			continue
		}
		// Only routes the linker actually evaluated. It declines, with reasons,
		// to reason about UI routes, GraphQL operations, methodless routes and
		// generic paths like /health — 3,471 of the estate's 9,135 server routes
		// — and counting those in the denominator makes the proportion describe
		// a population the numerator was never drawn from. A finding reading
		// "3,391 of 4,403 unmatched" was mixing routes that were checked with
		// routes that were never reached.
		repo := f.Repo
		if repo == "" {
			repo = "(unlabeled)"
		}
		if f.PropAny("unmatched_by_clients") != true && f.PropAny("matched_by_clients") != true {
			// Counted, not dropped. A route the linker declined belongs in the
			// finding as a coverage fact — it is the difference between "39
			// endpoints, 27 unused" and "39 of 866 endpoints could be assessed
			// at all", and a reader who is not told the second will read the
			// first as a statement about the repository.
			declined[repo]++
			continue
		}
		served[repo]++
		if servedIn[repo] == nil {
			servedIn[repo] = map[string]int{}
		}
		servedIn[repo][f.File]++
	}

	byRepo := map[string][]route{}
	for _, f := range store.ByKind(facts.KindRoute) {
		if f.Props == nil || f.PropAny("unmatched_by_clients") != true {
			continue
		}
		if level, _ := f.PropAny(providers.PropResolutionLevel).(string); level == providers.LevelRuntimeObserved {
			continue
		}
		repo := f.Repo
		if repo == "" {
			repo = "(unlabeled)"
		}
		byRepo[repo] = append(byRepo[repo], route{name: f.Name, file: f.File, label: routeLabel(f)})
	}
	unreached := unreachedFindings(store)
	if len(byRepo) == 0 {
		return append(unreached, duplicates...), nil
	}

	repos := make([]string, 0, len(byRepo))
	for r := range byRepo {
		repos = append(repos, r)
	}
	sort.Strings(repos)

	var insights []facts.Insight
	for _, repo := range repos {
		routes := byRepo[repo]
		sort.Slice(routes, func(i, j int) bool { return routes[i].label < routes[j].label })

		total := served[repo]
		share := 0.0
		if total > 0 {
			share = float64(len(routes)) / float64(total)
		}
		unexamined := ""
		if declined[repo] > 0 {
			unexamined = fmt.Sprintf(
				" A further %d route(s) in %s were not assessed at all — page routes, GraphQL "+
					"operations, routes with no HTTP verb and generic paths, which this pass "+
					"declines to reason about. They are outside both counts above, and their "+
					"use is unknown rather than absent.",
				declined[repo], repo)
		}

		// When nearly everything is unmatched, the snapshot is telling you about
		// its own client set rather than about the endpoints. The monolith above
		// reports 3,392 of 3,732 that way — its real callers are a browser, an
		// external integrator and a mobile app, none of which is a repository.
		// Presenting that as a list of dead-endpoint candidates is how a finding
		// gets ignored wholesale, and the ignoring is deserved.
		if share >= incompleteClientsShare || len(routes) > maxActionableCandidates {
			insights = append(insights, facts.Insight{
				Title: fmt.Sprintf("Client coverage for %s is too thin to name unused endpoints (%d of %d unmatched)",
					repo, len(routes), total),
				Description: fmt.Sprintf(
					"%d of %s's %d server route(s) — %.0f%% — matched no client call site in any loaded repo. "+
						"At that proportion this describes the snapshot's client set, not the endpoints: an API "+
						"whose callers are browsers, third-party integrators or apps outside the estate cannot "+
						"be assessed this way. Add the missing client repositories and regenerate before reading "+
						"any of these as dead.%s",
					len(routes), repo, total, share*100, unexamined),
				Confidence: 0.9,
				// Where the unmatched routes are, by route file, most first. The list
				// is withheld because it is not a list of dead endpoints; how it
				// spreads across the API is still worth reading, and it is how a
				// question about one area of the API is answered from here.
				Evidence: coverageByFile(routes, servedIn[repo]),
				Actions: []string{
					"Add the repositories that call this API and regenerate; the list is not meaningful until then",
					"List one file's unmatched routes: query_facts(kind=route, prop=unmatched_by_clients, prop_value=true, file=<file>)",
				},
			})
			continue
		}

		insights = append(insights, facts.Insight{
			Title: fmt.Sprintf("Unused endpoint candidates: %d of %d route(s) in %s have no caller among loaded clients",
				len(routes), total, repo),
			Description: fmt.Sprintf("%d of %s's %d server route(s) matched no client call site in any loaded repo. "+
				"These are candidates unused by the loaded clients ONLY — the snapshot cannot see consumers it "+
				"does not contain (admin/ops scripts, cron jobs, webhooks, third-party API clients, mobile deep "+
				"links). Verify each against those before removing the endpoint.%s Samples: %s.",
				len(routes), repo, total, unexamined, sampleList(routes)),
			Confidence: 0.6,
			Evidence:   evidenceFor(routes),
			Actions: []string{
				"Verify each candidate against consumers not in the snapshot before removing the endpoint",
				"Append any missing client repo and regenerate to shrink the list to truly-unused routes",
			},
		})
	}
	return append(append(insights, unreached...), duplicates...), nil
}

// unreachedFindings reports, per repository, the routes loaded clients call only from
// code the application cannot run (unreached_by_clients): an API method no page,
// component or store reaches. The call is in the source and is never sent. It is its
// own finding rather than part of the unmatched one, because the claim differs: the
// unmatched routes have no call at all, these have calls that never run, and the
// remedy removes the client code with them.
func unreachedFindings(store *facts.Store) []facts.Insight {
	byRepo := map[string][]route{}
	for _, f := range store.ByKind(facts.KindRoute) {
		if f.Props == nil || f.PropAny("unreached_by_clients") != true {
			continue
		}
		repo := f.Repo
		if repo == "" {
			repo = "(unlabeled)"
		}
		byRepo[repo] = append(byRepo[repo], route{name: f.Name, file: f.File, label: routeLabel(f)})
	}
	if len(byRepo) == 0 {
		return nil
	}
	// Which never-running client calls name each route, for the evidence.
	callers := map[string][]string{} // repo \x00 METHOD path → "file:line (caller)"
	for _, f := range store.ByKind(facts.KindRoute) {
		if f.PropAny(facts.PropRole) != facts.RoleClient || f.PropAny(facts.PropClientReachable) != false {
			continue
		}
		refs, _ := f.PropAny(facts.PropMatchedRoutes).([]any)
		for _, r := range refs {
			m, _ := r.(map[string]any)
			repo, _ := m["repo"].(string)
			method, _ := m["method"].(string)
			name, _ := m["name"].(string)
			site := fmt.Sprintf("%s:%d", f.File, f.Line)
			if caller := f.PropString(facts.PropCaller); caller != "" {
				site += " (" + caller + ")"
			}
			key := repo + "\x00" + strings.ToUpper(method) + " " + name
			callers[key] = append(callers[key], site)
		}
	}

	repos := make([]string, 0, len(byRepo))
	for r := range byRepo {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	var out []facts.Insight
	for _, repo := range repos {
		routes := byRepo[repo]
		sort.Slice(routes, func(i, j int) bool { return routes[i].label < routes[j].label })
		listed := routes
		if len(listed) > maxActionableCandidates {
			listed = listed[:maxActionableCandidates]
		}
		evidence := make([]facts.Evidence, 0, len(listed))
		for _, r := range listed {
			detail := r.label + " is called only from client code that never runs"
			if sites := callers[repo+"\x00"+upperMethod(r.label)]; len(sites) > 0 {
				sort.Strings(sites)
				detail += ": " + sites[0]
				if len(sites) > 1 {
					detail += fmt.Sprintf(" and %d more", len(sites)-1)
				}
			}
			evidence = append(evidence, facts.Evidence{Fact: r.name, File: r.file, Detail: detail})
		}
		more := ""
		if len(routes) > len(listed) {
			more = fmt.Sprintf(" The evidence lists %d of them; query_facts(kind=route, prop=unreached_by_clients, prop_value=true) lists all.", len(listed))
		}
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("Endpoints called only from client code that never runs: %d in %s", len(routes), repo),
			Description: fmt.Sprintf(
				"%d of %s's route(s) are called by a loaded client, but every such call sits in client code "+
					"nothing the application runs reaches: an API method no page, component or store calls. "+
					"The call is in the source and the request is never sent, so these are dead in practice "+
					"though not unmatched. Confirm against consumers outside the snapshot, and remove the "+
					"client code that names them with them.%s", len(routes), repo, more),
			Confidence: 0.6,
			Evidence:   evidence,
			Actions: []string{
				"Check each route's client caller: if nothing should call it, remove the endpoint and the client method together",
				"If the client method should be called, the feature that would call it is missing",
			},
		})
	}
	return out
}

// route carries the three things an unmatched route contributes downstream: the fact
// NAME, which is the only string the snapshot diff can match; the FILE, which is what
// scopes an insight to a repo in a multi-repo snapshot; and the human-readable LABEL
// used in the title and description.
//
// Keeping the name separate from the label is the point. The label is "GET /orders"
// while the fact is named "/orders", so citing the label as evidence matched nothing:
// the diff could never attribute a newly-unused route to the change that caused it,
// and query_insights(repo=…) — which scopes by evidence file or path — returned
// nothing at all for the one explainer that exists only in multi-repo snapshots.
type route struct {
	name  string
	file  string
	label string
}

// routeLabel renders a route fact as "METHOD /path" when a method prop is present,
// else just its name.
func routeLabel(f facts.Fact) string {
	if m, ok := f.PropAny("method").(string); ok && m != "" {
		return m + " " + f.Name
	}
	return f.Name
}

// sampleList renders up to maxSamples route labels, noting how many were elided.
func sampleList(routes []route) string {
	labels := make([]string, 0, len(routes))
	for _, r := range routes {
		labels = append(labels, r.label)
	}
	if len(labels) > maxSamples {
		return fmt.Sprintf("%v (+%d more)", labels[:maxSamples], len(labels)-maxSamples)
	}
	return fmt.Sprintf("%v", labels)
}

// evidenceFor attaches up to maxSamples routes as insight evidence, keyed on the route
// fact's name so the delta can attribute it, with the method kept in the detail so two
// verbs on one path stay distinguishable.
func evidenceFor(routes []route) []facts.Evidence {
	n := len(routes)
	if n > maxSamples {
		n = maxSamples
	}
	out := make([]facts.Evidence, 0, n)
	for _, r := range routes[:n] {
		detail := "no loaded client calls this route"
		if r.label != r.name {
			detail = r.label + " — " + detail
		}
		out = append(out, facts.Evidence{Fact: r.name, File: r.file, Detail: detail})
	}
	return out
}

// coverageByFile states, for the route files holding the most unmatched routes, how
// many of the routes assessed there match no client.
func coverageByFile(routes []route, served map[string]int) []facts.Evidence {
	unmatched := map[string]int{}
	for _, r := range routes {
		if r.file != "" { // a route with no file has nowhere to point
			unmatched[r.file]++
		}
	}
	files := make([]string, 0, len(unmatched))
	for f := range unmatched {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool {
		if unmatched[files[i]] != unmatched[files[j]] {
			return unmatched[files[i]] > unmatched[files[j]]
		}
		return files[i] < files[j]
	})
	if len(files) > maxSamples {
		files = files[:maxSamples]
	}
	out := make([]facts.Evidence, 0, len(files))
	for _, f := range files {
		out = append(out, facts.Evidence{
			File:   f,
			Detail: fmt.Sprintf("%d of %d assessed route(s) in this file match no client call site", unmatched[f], served[f]),
		})
	}
	return out
}

// pathParam matches a path parameter in either spelling a router accepts, so
// `/items/{id}`, `/items/{id:[0-9]+}` and `/items/:id` compare equal.
var pathParam = regexp.MustCompile(`\{[^}]*\}|/:[A-Za-z_][A-Za-z0-9_]*`)

// wrapsEndpoints are the route types that sit on a path beside its endpoint
// rather than competing with it.
var wrapsEndpoints = map[string]bool{
	"middleware": true, "layout": true, "loading": true, "error": true,
	"template": true, "not-found": true, "default": true,
}

// navigationRouters are the client-side routers: a route of theirs is a screen the
// application navigates to, matched in the browser or the app, and never an HTTP
// request a server answers. Keyed by framework, or by router where one framework
// prop covers both (`vue`).
var navigationRouters = map[string]bool{
	"react-router": true, "angular": true, "auto_route": true, "go_router": true, "vue-router": true,
}

// isNavigationRoute reports whether a route is a client-side screen rather than an
// HTTP endpoint.
func isNavigationRoute(f facts.Fact) bool {
	return navigationRouters[f.PropString("framework")] || navigationRouters[f.PropString("router")]
}

// duplicateRoutes reports a server route — one method on one path — registered in
// more than one file of a repository. A router serves one registration of a path
// and the other handler never runs, or refuses to start; which one wins depends on
// registration order, which a snapshot does not see. Either way two places claim
// one endpoint, and a change made in the wrong one changes nothing.
func duplicateRoutes(store *facts.Store) []facts.Insight {
	type site struct {
		file   string
		line   int
		module string // graph node of the declaring module
		repo   string
		path   string // repo-relative file
		golang bool
		mux    string // facts.PropMuxScope, "" when unknown
		// versions are the API versions it serves (facts.PropAPIVersions), nil when
		// the source states none.
		versions []string
	}
	moduleKeys := common.NewModuleKeys(store)
	byKey := map[string]map[string][]site{} // repo → "METHOD path" → registrations
	for _, f := range store.ByKind(facts.KindRoute) {
		if f.Props == nil {
			continue
		}
		if role, _ := f.PropAny("role").(string); role == "client" {
			continue
		}
		method, _ := f.PropAny("method").(string)
		if method == "" || f.File == "" {
			continue
		}
		// Middleware mounted on a prefix is not an endpoint: two files may each
		// guard the same prefix, and neither shadows the other. Nor is a Next.js
		// layout, loading or error file, which wraps the page on its path.
		if kind, _ := f.PropAny("type").(string); wrapsEndpoints[kind] || strings.EqualFold(method, "USE") {
			continue
		}
		// A hook (WordPress's add_action, add_filter) takes any number of callbacks,
		// and every one of them runs: a second registration adds, it shadows nothing.
		if f.PropString("type") == "hook" {
			continue
		}
		// A test registers on a server of its own, often the same path in every case;
		// what it registers is not the application's routing.
		if facts.IsTestPath(f.File) {
			continue
		}
		// A spec documents an endpoint and registers nothing: a spec beside the route
		// it describes, or two specs naming one health check, are not two
		// registrations a router has to choose between.
		switch f.PropString(facts.PropSource) {
		case facts.RouteSourceOpenAPI, facts.RouteSourceGRPCProto, facts.RouteSourceOpenAPITypeScript:
			continue
		}
		// A screen and an endpoint on one path are matched by different routers, one
		// in the browser and one on the server, so only screens compete with screens.
		namespace := ""
		if isNavigationRoute(f) {
			namespace = "screen "
		}
		key := namespace + strings.ToUpper(method) + " " + pathParam.ReplaceAllStringFunc(f.Name, func(m string) string {
			if strings.HasPrefix(m, "/") {
				return "/{}"
			}
			return "{}"
		})
		if byKey[f.Repo] == nil {
			byKey[f.Repo] = map[string][]site{}
		}
		s := site{file: f.File, line: f.Line, repo: f.Repo, path: repoRelative(f),
			golang: f.PropString("language") == "go", mux: f.PropString(facts.PropMuxScope),
			versions: f.PropStrings(facts.PropAPIVersions)}
		for _, r := range f.Relations {
			if r.Kind == facts.RelDeclares {
				s.module = moduleKeys.Key(f.Repo, r.Target)
				break
			}
		}
		byKey[f.Repo][key] = append(byKey[f.Repo][key], s)
	}
	repos := make([]string, 0, len(byKey))
	for r := range byKey {
		repos = append(repos, r)
	}
	sort.Strings(repos)

	var programs *goPrograms // built on first need: most repositories never ask
	var apps *appRoots
	competes := func(a, b site) bool {
		if a.file == b.file {
			return false
		}
		if disjointVersions(a.versions, b.versions) {
			return false
		}
		if !a.golang || !b.golang {
			if apps == nil {
				apps = newAppRoots(store)
			}
			return apps.share(a.repo, a.path, b.repo, b.path)
		}
		// Two muxes the extractor could name, and not the same one: each serves its
		// own registration of the path.
		if a.mux != "" && b.mux != "" && a.mux != b.mux {
			return false
		}
		if programs == nil {
			programs = newGoPrograms(store)
		}
		return programs.share(a.module, b.module)
	}

	var out []facts.Insight
	for _, repo := range repos {
		var keys []string
		competing := map[string][]site{}
		for key, sites := range byKey[repo] {
			in := make([]bool, len(sites))
			for i := range sites {
				for j := i + 1; j < len(sites); j++ {
					if competes(sites[i], sites[j]) {
						in[i], in[j] = true, true
					}
				}
			}
			var kept []site
			for i, s := range sites {
				if in[i] {
					kept = append(kept, s)
				}
			}
			if len(kept) > 0 {
				keys = append(keys, key)
				competing[key] = kept
			}
		}
		if len(keys) == 0 {
			continue
		}
		sort.Strings(keys)
		var evidence []facts.Evidence
		for _, key := range keys {
			if len(evidence) >= maxSamples {
				break
			}
			sites := competing[key]
			sort.Slice(sites, func(i, j int) bool {
				if sites[i].file != sites[j].file {
					return sites[i].file < sites[j].file
				}
				return sites[i].line < sites[j].line
			})
			where := make([]string, 0, len(sites))
			for _, s := range sites {
				where = append(where, fmt.Sprintf("%s:%d", s.file, s.line))
			}
			evidence = append(evidence, facts.Evidence{
				File:   sites[0].file,
				Line:   sites[0].line,
				Detail: fmt.Sprintf("%s is registered at %s", key, strings.Join(where, ", ")),
			})
		}
		label := repo
		if label == "" {
			label = "this repository"
		}
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("Routes registered in more than one file of %s: %d", label, len(keys)),
			Description: fmt.Sprintf(
				"%d method and path pair(s) in %s are registered in two or more files. A router serves one "+
					"registration of a path, so one of the handlers never runs (or the router refuses to start), "+
					"and which one depends on the order the files register in, which the snapshot does not see. "+
					"A change made to the handler that is not served changes nothing.", len(keys), label),
			Confidence: 0.6,
			Evidence:   evidence,
			Actions: []string{
				"Keep one registration of each pair and delete the other, with its handler if nothing else uses it",
			},
		})
	}
	return out
}

// goPrograms answers whether two Go packages can be linked into one program. A Go
// repository often builds many binaries, each serving its own mux: two of them both
// registering /metrics shadow nothing. A program is a main package and everything it
// imports, so two registrations compete only when some main package reaches both, or
// when either is reached by none (a library served by programs outside the snapshot,
// which may well mount both).
type goPrograms struct {
	reachedBy map[string][]int // module node -> indices of the main packages reaching it
}

func newGoPrograms(store *facts.Store) *goPrograms {
	graph := common.BuildModuleGraphExcluding(store, facts.CouplingSymbolRollup)
	keys := common.NewModuleKeys(store)
	var mains []string
	for _, m := range store.ByKind(facts.KindModule) {
		if m.PropString("language") == "go" && m.PropString("package") == "main" && !common.IsTestModule(m) {
			mains = append(mains, keys.Key(m.Repo, m.Name))
		}
	}
	sort.Strings(mains)
	p := &goPrograms{reachedBy: map[string][]int{}}
	for i, root := range mains {
		seen := map[string]bool{root: true}
		queue := []string{root}
		for len(queue) > 0 {
			n := queue[0]
			queue = queue[1:]
			p.reachedBy[n] = append(p.reachedBy[n], i)
			for _, next := range graph[n] {
				if !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
		}
	}
	return p
}

// disjointVersions reports whether two registrations serve no API version in common,
// which makes them two endpoints the request picks between. Unknown on either side,
// or a version-neutral one, is not known to be apart.
func disjointVersions(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 || slices.Contains(a, facts.VersionNeutral) || slices.Contains(b, facts.VersionNeutral) {
		return false
	}
	for _, v := range a {
		if slices.Contains(b, v) {
			return false
		}
	}
	return true
}

// appRoots answers the same question as goPrograms for the other languages, one
// level up: which application a file is part of. A monorepo holds several (a web
// app, a docs site, an admin, examples), each with its manifest, and each serves
// its own routes: two of them registering "/" shadow nothing. An app root is the
// directory of a package manifest (package.json, pubspec.yaml, Gemfile, a .NET
// project, ...), a file belongs to the nearest one above it, and one root uses
// another when a module under it imports or references a module under the other. Two
// registrations compete when some root that nothing else uses (an application)
// reaches both, or when either file is under no root at all.
type appRoots struct {
	roots     map[string]map[string]bool // repo -> root directories ("." for the repo root)
	reachedBy map[string][]string        // repo+"\x00"+root -> sorted applications reaching it
}

func newAppRoots(store *facts.Store) *appRoots {
	a := &appRoots{roots: map[string]map[string]bool{}, reachedBy: map[string][]string{}}
	addRoot := func(repo, dir string) {
		if a.roots[repo] == nil {
			a.roots[repo] = map[string]bool{}
		}
		a.roots[repo][path.Clean(dir)] = true
	}
	for _, f := range store.ByKind(facts.KindExtraction) {
		if f.Name == facts.ManifestsFact {
			for _, m := range f.PropStrings(facts.PropManifests) {
				addRoot(f.Repo, path.Dir(m))
			}
		}
	}
	modules := store.ByKind(facts.KindModule)
	for _, m := range modules {
		if m.PropAny("msbuild") != nil {
			addRoot(m.Repo, repoRelative(m))
		}
	}
	// Which root each module node sits under, and the edges between roots.
	keys := common.NewModuleKeys(store)
	nodeRoot := map[string]string{}
	// A test project references what it tests, often several applications at once;
	// it is not an application that serves them together.
	for _, m := range modules {
		if common.IsTestModule(m) {
			continue
		}
		if r, ok := a.rootOf(m.Repo, path.Join(repoRelative(m), "x")); ok {
			nodeRoot[keys.Key(m.Repo, m.Name)] = m.Repo + "\x00" + r
		}
	}
	uses := map[string]map[string]bool{}
	link := func(from, to string) {
		if from == "" || to == "" || from == to {
			return
		}
		if uses[from] == nil {
			uses[from] = map[string]bool{}
		}
		uses[from][to] = true
	}
	// Imports and project references only: a call resolved by name can land in a
	// sibling application that happens to declare the same name.
	for node, targets := range common.BuildModuleGraphExcluding(store, facts.CouplingSymbolRollup) {
		for _, t := range targets {
			link(nodeRoot[node], nodeRoot[t])
		}
	}
	for _, m := range modules {
		for _, r := range m.Relations {
			if r.Kind == facts.RelDependsOn {
				link(nodeRoot[keys.Key(m.Repo, m.Name)], nodeRoot[keys.Key(m.Repo, r.Target)])
			}
		}
	}
	used := map[string]bool{}
	for _, to := range uses {
		for t := range to {
			used[t] = true
		}
	}
	var all []string
	for repo, dirs := range a.roots {
		for d := range dirs {
			all = append(all, repo+"\x00"+d)
		}
	}
	sort.Strings(all)
	for _, app := range all {
		if used[app] {
			continue
		}
		seen := map[string]bool{app: true}
		queue := []string{app}
		for len(queue) > 0 {
			n := queue[0]
			queue = queue[1:]
			a.reachedBy[n] = append(a.reachedBy[n], app)
			next := make([]string, 0, len(uses[n]))
			for t := range uses[n] {
				next = append(next, t)
			}
			sort.Strings(next)
			for _, t := range next {
				if !seen[t] {
					seen[t] = true
					queue = append(queue, t)
				}
			}
		}
	}
	return a
}

// rootOf is the nearest app root above a repo-relative file.
func (a *appRoots) rootOf(repo, file string) (string, bool) {
	dirs := a.roots[repo]
	for dir := path.Dir(file); ; dir = path.Dir(dir) {
		if dirs[dir] {
			return dir, true
		}
		if dir == "." || dir == "/" {
			return "", false
		}
	}
}

// share reports whether one application can serve both files. A file under no root,
// or a root no application reaches (a cycle of roots using each other), is not known
// to be apart from anything.
func (a *appRoots) share(repoA, fileA, repoB, fileB string) bool {
	ra, okA := a.rootOf(repoA, fileA)
	rb, okB := a.rootOf(repoB, fileB)
	if !okA || !okB {
		return true
	}
	pa, pb := a.reachedBy[repoA+"\x00"+ra], a.reachedBy[repoB+"\x00"+rb]
	if len(pa) == 0 || len(pb) == 0 {
		return true
	}
	for _, x := range pa {
		for _, y := range pb {
			if x == y {
				return true
			}
		}
	}
	return false
}

// repoRelative is a fact's file without the repo label a multi-repo snapshot adds.
func repoRelative(f facts.Fact) string {
	if f.RepoPrefixed {
		return strings.TrimPrefix(f.File, f.Repo+"/")
	}
	return f.File
}

// share reports whether one program can hold both modules. Both lists are ascending.
func (p *goPrograms) share(a, b string) bool {
	pa, pb := p.reachedBy[a], p.reachedBy[b]
	if len(pa) == 0 || len(pb) == 0 {
		return true
	}
	for i, j := 0, 0; i < len(pa) && j < len(pb); {
		switch {
		case pa[i] == pb[j]:
			return true
		case pa[i] < pb[j]:
			i++
		default:
			j++
		}
	}
	return false
}

// upperMethod upper-cases the method of a "METHOD path" label, the spelling the
// linker's matched routes carry.
func upperMethod(label string) string {
	if method, path, ok := strings.Cut(label, " "); ok {
		return strings.ToUpper(method) + " " + path
	}
	return label
}
