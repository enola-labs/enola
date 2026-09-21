// Package queryloops reports a database query issued once per iteration of a
// data-sized loop.
//
// It reports the shape it can prove and deliberately not the one everybody
// means by "N+1". The pitched detector was `record.association` inside a loop,
// and the funnel on a large Rails monolith ends at zero: 1,698 association-name
// reads in unbounded loops, 62 once bare `self.assoc` reads are dropped (Rails
// memoises those), 8 once thread-local receivers like `Current.company` are
// dropped, 7 after eager-loading — and all seven false.
// `Requisition#send_pusher_event!` reads `channel.trigger` where `channel` is a
// `PusherChannel`, matched only because `belongs_to :trigger` exists on an
// unrelated model. The graph has no receiver type inference for Ruby, so
// `candidate.posts` and `client.post` are the same string to it.
//
// A class-level query has no such problem: the receiver IS the type. When
// `AccessLevel.find_by` appears inside an unbounded loop, `AccessLevel` is a
// model this graph already knows and `find_by` is a query — nothing is
// inferred. 97 of these on that monolith.
//
// It also needs no eager-load suppression, and that is a property of the shape
// rather than a shortcut: `includes` cannot help a class-level `find_by`.
//
// The association-read shape does need two suppressions, both measured on the
// same monolith after the first tranche of fixes was judged: a read on an
// association the method has preloaded (`includes`, `preload`, `eager_load`)
// is a cache hit per element, not a query, and a read on a local the method
// typed with `new` is a record that was never saved, so its associations build
// in memory. Both come from facts the extractor now states (`preloads`,
// `unpersisted_locals`) rather than from anything inferred here.
//
// Rails states most preloads one hop away from the loop, so the block binding
// carries the chain the iterated relation was built from
// (`Current.company.users.allowed_to_login.preload`), and the walk back to the
// association collects the preloads stated on the way: by a scope on the
// element's model, by a same-class method the chain calls, by the method body
// itself. A collection that is nothing but a method parameter is typed by its
// name alone; that finding stays, at half the confidence and saying why. A
// method that hands its reads to BatchLoader.for is batched by construction.
//
// A finding also says where the loop runs, read from the file's place in the
// Rails layout: the request path, a background job, an admin action, shared
// code, or a one-off task (rake, maintenance tasks, migrations, seeders,
// development helpers). One-off tasks are informational, still reported and
// never graded; loops in spec and test files are not findings at all, since
// fixtures are iterated by design. Whether a loop is hot is still not
// measured; the surface is what the path states.
package queryloops

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

type Explainer struct{}

func New() *Explainer { return &Explainer{} }

func (e *Explainer) Name() string { return "query-loops" }

// queryMethods are ActiveRecord class methods that reach the database. A class
// method that does not is not a finding — `AccessLevel.human_name` is free.
var queryMethods = map[string]bool{
	"find": true, "find_by": true, "find_by!": true, "find_or_create_by": true,
	"find_or_create_by!": true, "find_or_initialize_by": true, "where": true,
	"first": true, "last": true, "count": true, "exists?": true, "pluck": true,
	"sum": true, "create": true, "create!": true, "update": true, "update!": true,
	"destroy": true, "destroy_all": true, "save": true, "save!": true,
	"order": true, "find_each": true, "take": true, "average": true, "minimum": true,
	"maximum": true, "delete_all": true, "upsert": true, "insert": true,
}

type finding struct {
	symbol string
	file   string
	repo   string
	call   string
	depth  int
	// element and target are set only for the association-read shape: the type
	// the block parameter holds, and what the association it reads points at.
	element string
	target  string
	// write marks a persistence call rather than a read: it cannot be
	// eager-loaded away, so it needs a different fix and says so.
	write bool
	// weak marks an element typed by nothing but a parameter's name matching an
	// association: whether the caller preloaded it is not visible here.
	weak bool
}

// surface is where a file runs, read from the Rails layout. It decides how a
// finding is graded, never whether it is one.
type surface struct {
	name     string
	phrase   string
	oneOff   bool
	excluded bool
}

var surfaces = []struct {
	prefix string
	surface
}{
	{"spec/", surface{name: "test", excluded: true}},
	{"test/", surface{name: "test", excluded: true}},
	{"db/migrate/", surface{name: "task", phrase: "runs once, in a migration", oneOff: true}},
	{"db/seeds", surface{name: "task", phrase: "runs once, as a seed", oneOff: true}},
	{"lib/tasks/", surface{name: "task", phrase: "runs as a one-off task", oneOff: true}},
	{"app/tasks/", surface{name: "task", phrase: "runs as a one-off maintenance task", oneOff: true}},
	{"lib/development", surface{name: "task", phrase: "runs as a development helper", oneOff: true}},
	{"app/services/development/", surface{name: "task", phrase: "runs as a development helper", oneOff: true}},
	{"app/controllers/", surface{name: "request", phrase: "runs on the request path"}},
	{"app/graphql/", surface{name: "request", phrase: "runs on the request path"}},
	{"app/resources/", surface{name: "request", phrase: "runs on the request path"}},
	{"app/serializers/", surface{name: "request", phrase: "runs on the request path"}},
	{"app/presenters/", surface{name: "request", phrase: "runs on the request path"}},
	{"app/channels/", surface{name: "request", phrase: "runs on the request path"}},
	{"app/mcp_public_tools/", surface{name: "request", phrase: "runs on the request path"}},
	{"app/policies/", surface{name: "request", phrase: "runs on the request path"}},
	{"app/helpers/", surface{name: "request", phrase: "runs on the request path"}},
	{"app/views/", surface{name: "request", phrase: "runs on the request path"}},
	{"app/components/", surface{name: "request", phrase: "runs on the request path"}},
	{"app/jobs/", surface{name: "job", phrase: "runs as a background job"}},
	{"app/workers/", surface{name: "job", phrase: "runs as a background job"}},
	{"app/mailers/", surface{name: "job", phrase: "runs as a background job"}},
	{"app/importers/", surface{name: "job", phrase: "runs as an import job"}},
	{"lib/imports/", surface{name: "job", phrase: "runs as an import job"}},
	{"app/avo/", surface{name: "admin", phrase: "runs as an admin action"}},
	{"app/admin/", surface{name: "admin", phrase: "runs as an admin action"}},
}

var sharedSurface = surface{name: "shared", phrase: "runs wherever it is called from"}

// surfaceOf reads the surface from the file path. A union snapshot prefixes
// paths with the repo label, so leading segments are dropped until the path
// starts at a Rails root (app/, lib/, db/, spec/, test/); a path that never
// does, or that matches no entry, is shared.
func surfaceOf(file string) surface {
	path := file
	for !startsAtRoot(path) {
		i := strings.Index(path, "/")
		if i < 0 {
			return sharedSurface
		}
		path = path[i+1:]
	}
	for _, s := range surfaces {
		if strings.HasPrefix(path, s.prefix) {
			return s.surface
		}
	}
	return sharedSurface
}

func startsAtRoot(path string) bool {
	for _, root := range []string{"app/", "lib/", "db/", "spec/", "test/"} {
		if strings.HasPrefix(path, root) {
			return true
		}
	}
	return false
}

// relationChain are the ActiveRecord relation methods a chain passes through
// without changing what its elements are.
var relationChain = map[string]bool{
	"where": true, "not": true, "order": true, "reorder": true, "limit": true,
	"offset": true, "includes": true, "preload": true, "eager_load": true,
	"joins": true, "left_joins": true, "left_outer_joins": true, "distinct": true,
	"group": true, "having": true, "select": true, "references": true,
	"merge": true, "unscope": true, "unscoped": true, "readonly": true,
	"lock": true, "none": true, "all": true, "reverse_order": true,
	"rewhere": true, "or": true, "and": true, "extending": true, "strict_loading": true,
	"in_batches": true, "find_each": true, "find_in_batches": true, "each": true,
	"to_a": true, "load": true, "reload": true, "records": true,
}

// preloadIndex answers where a preload was stated other than in the loop's own
// body: scopes by (model, name), and methods by their qualified name.
type preloadIndex struct {
	scopes  map[string]map[string][]string
	methods map[string][]string
}

func buildPreloadIndex(store *facts.Store) preloadIndex {
	idx := preloadIndex{scopes: map[string]map[string][]string{}, methods: map[string][]string{}}
	for _, fact := range store.ByKind(facts.KindSymbol) {
		if lang, _ := fact.PropAny("language").(string); lang != "ruby" {
			continue
		}
		preloads := propStrings(fact.PropAny("preloads"))
		if isScope, _ := fact.PropAny("scope").(bool); isScope {
			model, _ := fact.PropAny("model").(string)
			name := strings.TrimPrefix(fact.Name, "scope:")
			if model == "" || name == "" {
				continue
			}
			if idx.scopes[model] == nil {
				idx.scopes[model] = map[string][]string{}
			}
			idx.scopes[model][name] = preloads
			continue
		}
		if len(preloads) > 0 {
			idx.methods[fact.Name] = preloads
		}
	}
	return idx
}

// resolution is what a block binding's chain says about the loop's elements.
type resolution struct {
	element   string
	preloaded map[string]bool
	weak      bool
}

// resolveChain walks a binding's chain from the iterator inward. Relation
// methods are skipped; the first segment that names exactly one association
// fixes the element type, and a scope on that model or a same-class method
// passed on the way contributes the preloads it states. `owner` is the class of
// the method the loop is in, so `users.each` joins `Owner#users` when the class
// defines it. A collection that resolves only through a name that is one of the
// method's own parameters is typed by that name alone, and says so.
func resolveChain(collection, owner string, params, models map[string]bool, associations associationIndex, preloads preloadIndex) (resolution, bool) {
	segments := strings.Split(collection, ".")
	res := resolution{preloaded: map[string]bool{}}
	base := -1
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		if relationChain[seg] {
			continue
		}
		if elements := associations.targetsOf(seg); len(elements) == 1 {
			res.element = elements[0]
			base = i
			break
		}
		if preloads.isScope(seg) {
			continue
		}
		if i == 0 && (models[demodulize(seg)] || preloads.scopes[demodulize(seg)] != nil) {
			res.element = demodulize(seg)
			base = i
			break
		}
		return resolution{}, false
	}
	if base < 0 {
		return resolution{}, false
	}
	if base == 0 && len(segments) == 1 && params[segments[0]] {
		res.weak = true
	}
	if base == 0 && owner != "" {
		for _, sep := range []string{"#", "."} {
			for _, p := range preloads.methods[owner+sep+segments[0]] {
				res.preloaded[p] = true
			}
		}
	}
	for i := base + 1; i < len(segments); i++ {
		if scoped, ok := preloads.scopes[res.element][segments[i]]; ok {
			for _, p := range scoped {
				res.preloaded[p] = true
			}
		}
	}
	return res, true
}

// isScope reports whether name is a scope on any model, which lets the walk
// continue inward to the association or constant that types the elements; the
// scope's preloads join only once that model is known.
func (i preloadIndex) isScope(name string) bool {
	for _, byName := range i.scopes {
		if _, ok := byName[name]; ok {
			return true
		}
	}
	return false
}

func ownerOf(symbol string) string {
	if i := strings.LastIndexAny(symbol, "#."); i >= 0 {
		return symbol[:i]
	}
	return ""
}

// persistMethods write to the database on an instance. Kept apart from
// queryMethods, which are class-level and include reads.
var persistMethods = map[string]bool{
	"save": true, "save!": true, "update": true, "update!": true,
	"destroy": true, "delete": true, "touch": true, "increment!": true,
	"decrement!": true, "update_column": true, "update_columns": true,
}

// associationIndex answers the two questions the association-read shape needs:
// what a collection's elements are, and whether a method is an association on
// that type. Both come from association facts the graph already holds.
type associationIndex struct {
	byName map[string][]string
	onType map[string]map[string]string
}

// targetsOf returns the distinct types a collection name resolves to. Distinct,
// because the same association declared identically on several models is not
// ambiguity — `has_many :users` pointing at User from twenty models still says
// the element is a User.
func (i associationIndex) targetsOf(collection string) []string {
	seen := map[string]bool{}
	var out []string
	for _, target := range i.byName[collection] {
		if seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
	}
	sort.Strings(out)
	return out
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

func (i associationIndex) on(model, method string) string {
	if byModel, ok := i.onType[model]; ok {
		return byModel[method]
	}
	return ""
}

func candidates(store *facts.Store) []finding {
	// Two shapes, two prerequisites. The class-level shape needs the model
	// classes; the association-read shape needs association facts and block
	// bindings. Gating both on models would have silenced the association shape
	// wherever storage facts were absent, which is a repository that has
	// associations and no tables — rare, and exactly the kind of silent zero
	// this explainer exists downstream of.
	models := modelClasses(store)
	associations := buildAssociationIndex(store)
	if len(models) == 0 && len(associations.byName) == 0 {
		return nil
	}
	preloadsElsewhere := buildPreloadIndex(store)

	var found []finding
	for _, fact := range store.ByKind(facts.KindSymbol) {
		// The association-read shape: an association read on the element of a
		// collection. `form_questions.each { |q| q.form_answers }` is a query per
		// iteration only because `q` is a FormQuestion, and the block binding is
		// what says so. Without it the receiver is untyped and `client.post`
		// matches an association named `post` on an unrelated model — which is
		// exactly how the first attempt produced seven candidates and zero true
		// findings.
		// A local typed by assignment resolves the same way a block parameter
		// does, and gives the same guarantee: the receiver's type is stated in
		// the source rather than guessed from a name.
		preloaded := stringSet(propStrings(fact.PropAny("preloads")))
		unpersisted := stringSet(propStrings(fact.PropAny("unpersisted_locals")))
		batched, _ := fact.PropAny("batch_loader").(bool)
		params := stringSet(propStrings(fact.PropAny("params")))
		owner := ownerOf(fact.Name)
		for _, typed := range propStrings(fact.PropAny("local_types")) {
			name, class, ok := strings.Cut(typed, "=")
			if !ok {
				continue
			}
			for _, call := range propStrings(fact.PropAny("calls_in_loop")) {
				receiver, method, dotted := strings.Cut(call, ".")
				if !dotted || receiver != name {
					continue
				}
				// A write per iteration is worse than a read per iteration: it
				// cannot be eager-loaded away, and each one is a round trip that
				// a single update_all or insert_all would replace. Reported only
				// on a typed receiver, so `client.save` is not mistaken for a
				// record being persisted.
				if models[demodulize(class)] && persistMethods[method] {
					found = append(found, finding{
						symbol: fact.Name, file: fact.File, repo: fact.Repo,
						call: call, depth: propInt(fact.PropAny("loop_depth")),
						element: demodulize(class), write: true,
					})
					continue
				}
				// A record typed by `new` was never saved; reading its associations
				// builds in memory. A preloaded association is a cache hit.
				if unpersisted[name] || preloaded[method] || batched {
					continue
				}
				if target := associations.on(demodulize(class), method); target != "" {
					found = append(found, finding{
						symbol: fact.Name, file: fact.File, repo: fact.Repo,
						call: call, depth: propInt(fact.PropAny("loop_depth")),
						element: demodulize(class), target: target,
					})
				}
			}
		}
		for _, binding := range propStrings(fact.PropAny("block_bindings")) {
			param, collection, ok := strings.Cut(binding, "=")
			if !ok {
				continue
			}
			// A collection name declared on more than one model gives the element
			// more than one candidate type, and picking one would be the
			// name-coincidence guess that produced seven false findings before.
			// A missing edge beats a wrong one.
			res, ok := resolveChain(collection, owner, params, models, associations, preloadsElsewhere)
			if !ok || batched {
				continue
			}
			for _, call := range propStrings(fact.PropAny("calls_in_loop")) {
				receiver, method, dotted := strings.Cut(call, ".")
				if !dotted || receiver != param {
					continue
				}
				if preloaded[method] || res.preloaded[method] {
					continue
				}
				if target := associations.on(res.element, method); target != "" {
					found = append(found, finding{
						symbol: fact.Name, file: fact.File, repo: fact.Repo,
						call: call, depth: propInt(fact.PropAny("loop_depth")),
						element: res.element, target: target, weak: res.weak,
					})
				}
			}
		}
	}
	for _, fact := range store.ByKind(facts.KindSymbol) {
		if lang, _ := fact.PropAny("language").(string); lang != "ruby" {
			continue
		}
		if len(models) == 0 {
			continue
		}
		depth := propInt(fact.PropAny("loop_depth"))
		for _, call := range propStrings(fact.PropAny("calls_in_loop")) {
			receiver, method, ok := strings.Cut(call, ".")
			if !ok || !queryMethods[method] {
				continue
			}
			if !models[demodulize(receiver)] {
				continue
			}
			found = append(found, finding{
				symbol: fact.Name, file: fact.File, repo: fact.Repo,
				call: call, depth: depth,
			})
		}
	}
	if len(found) == 0 {
		return nil
	}
	// One finding per (symbol, call): a loop body reporting the same read twice
	// is the explainer counting its own passes, not two problems.
	deduped := found[:0]
	seenFinding := map[string]bool{}
	for _, f := range found {
		key := f.symbol + "\x00" + f.call
		if seenFinding[key] {
			continue
		}
		seenFinding[key] = true
		deduped = append(deduped, f)
	}
	found = deduped

	// Deeper loops first: a query at depth 2 runs a product of two collections,
	// which is a different order of problem from one at depth 1.
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].depth != found[j].depth {
			return found[i].depth > found[j].depth
		}
		return found[i].symbol < found[j].symbol
	})
	return found
}

// ClaimedSymbols is the set of symbols this explainer reports on, computed by the
// same pass that produces the findings themselves.
//
// It exists so a broader analyzer can stay silent where this one speaks. The
// performance analyzer estimates a per-iteration I/O call from a keyword gate over
// ten languages; this explainer answers the same question for Ruby from the
// receiver's type, and was measured down from 1,698 candidates to 97. Where both
// can see a symbol, the measured answer is the one worth reporting, and two
// findings on one loop read as two problems.
//
// It is deliberately a direct call rather than a prop an annotator stamps or an
// insight the consumer reads back. runExplainers is explicit that one explainer's
// insights may never depend on another's having run, or the snapshot would depend
// on registration order; and a prop would make this explainer's output depend on
// whether that one is enabled in config. A pure function of the store has neither
// problem, at the cost of running the candidate pass twice.
//
// The filter below is the emission loop's own: a surface this explainer excludes
// is one it does not report, and so not one it claims.
func ClaimedSymbols(store *facts.Store) map[string]struct{} {
	reported, _ := reportable(store)
	claimed := make(map[string]struct{}, len(reported))
	for _, f := range reported {
		claimed[f.symbol] = struct{}{}
	}
	return claimed
}

// reportable is the findings this explainer actually files, and the count it leaves
// to the rollup: candidates, minus the surfaces it refuses to report, capped.
//
// The cap is inside it rather than applied by Explain, and that is load-bearing.
// A claim silences the performance analyzer on that symbol, so claiming a finding
// this explainer does not report would leave a loop that NEITHER reports — the cap
// would quietly become a hole instead of a summary. Filtering before capping
// matters for the same reason: capping first would spend budget on excluded
// surfaces and emit fewer than the cap.
func reportable(store *facts.Store) ([]finding, int) {
	all := candidates(store)
	kept := all[:0]
	for _, f := range all {
		if surfaceOf(f.file).excluded {
			continue
		}
		kept = append(kept, f)
	}
	// Per repository: a single budget spent in rank order is spent by whichever
	// repository ranks first, and the others read as clean when nobody looked.
	return common.CapByRepo(kept, func(f finding) string { return f.repo })
}

func (e *Explainer) Explain(_ context.Context, store *facts.Store) ([]facts.Insight, error) {
	// candidates() ranks deepest loop first, which is the order the budget is spent
	// in: a query at depth 2 runs a product of two collections and is a different
	// order of problem from one at depth 1.
	found, omitted := reportable(store)

	out := make([]facts.Insight, 0, len(found))
	for _, f := range found {
		description := fmt.Sprintf(
			"%s calls %s inside a loop nested %d deep. The receiver is a model this "+
				"graph knows and the method reaches the database, so the query count "+
				"grows with the collection. Eager loading cannot help a class-level "+
				"query — the fix is to read the set once outside the loop.",
			f.symbol, f.call, f.depth)
		actions := []string{
			"read the records once before the loop and index them in memory",
			"confirm with a query-count test that fails against the current code",
		}
		if f.write {
			description = fmt.Sprintf(
				"%s calls %s inside a loop nested %d deep, on a %s. A write per "+
					"iteration is a round trip per element and cannot be eager-loaded "+
					"away — the fix is a single bulk update rather than a preload.",
				f.symbol, f.call, f.depth, f.element)
			actions = []string{
				"replace the per-element write with update_all or insert_all",
				"confirm with a query-count test that fails against the current code",
			}
		} else if f.element != "" {
			// The association-read shape names its own fix, because eager loading
			// is exactly what solves it — unlike the class-level shape, where
			// `includes` cannot help.
			description = fmt.Sprintf(
				"%s reads %s inside a loop nested %d deep. The block parameter holds a %s, "+
					"so each iteration loads its %s — a query per element. Unlike a "+
					"class-level query this one eager-loads away.",
				f.symbol, f.call, f.depth, f.element, f.target)
			actions = []string{
				fmt.Sprintf("eager-load the association on the collection: includes(:%s)",
					strings.SplitN(f.call, ".", 2)[1]),
				"confirm with a query-count test that fails against the current code",
			}
		}
		// Below 1.0 and deliberately so: the loop is measured, the receiver is
		// measured, and whether the loop is hot is not. This is a candidate to
		// verify against a query count, which is the one oracle available here.
		where := surfaceOf(f.file)
		confidence := 0.8
		evidence := []facts.Evidence{{
			File:   f.file,
			Symbol: f.symbol,
			Detail: fmt.Sprintf("%s at loop depth %d", f.call, f.depth),
		}, {
			File:   f.file,
			Symbol: f.symbol,
			Detail: fmt.Sprintf("surface: %s, %s", where.name, where.phrase),
		}}
		if where.oneOff {
			confidence = 0.5
		}
		if f.weak {
			confidence = 0.5
			evidence = append(evidence, facts.Evidence{
				File:   f.file,
				Symbol: f.symbol,
				Detail: fmt.Sprintf("the collection arrives as the parameter %q and is typed by that name alone; a preload on the caller's relation would make this silent", strings.SplitN(f.call, ".", 2)[0]),
			})
		}
		out = append(out, facts.Insight{
			Title:         fmt.Sprintf("%s issues %s once per iteration", f.symbol, f.call),
			Description:   description,
			Confidence:    confidence,
			Evidence:      evidence,
			Actions:       actions,
			Informational: where.oneOff,
		})
	}
	if omitted > 0 {
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("Additional per-iteration queries: %d more", omitted),
			Description: fmt.Sprintf(
				"%d further loops issue a query per iteration and are not listed individually. They sit "+
					"at the same or shallower loop depth as those above, which is the order the budget is "+
					"spent in, and are candidates on the same footing.", omitted),
			Confidence: 0.5,
			Actions:    []string{"Fix the deepest loops first; they multiply the collection sizes"},
		})
	}
	return out, nil
}

// modelClasses is the set of class names this graph knows to be models, keyed
// without namespace. Being able to answer that question is the entire reason
// this explainer works where the association-read one did not.
func modelClasses(store *facts.Store) map[string]bool {
	models := map[string]bool{}
	for _, fact := range store.ByKind(facts.KindStorage) {
		if kind, _ := fact.PropAny("storage_kind").(string); kind != "model" {
			continue
		}
		models[demodulize(fact.Name)] = true
	}
	return models
}

func demodulize(name string) string {
	if i := strings.LastIndex(name, "::"); i >= 0 {
		return name[i+2:]
	}
	return name
}

func propStrings(v any) []string {
	switch value := v.(type) {
	case []string:
		return value
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func propInt(v any) int {
	switch value := v.(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	}
	return 0
}

func buildAssociationIndex(store *facts.Store) associationIndex {
	index := associationIndex{byName: map[string][]string{}, onType: map[string]map[string]string{}}
	for _, fact := range store.ByKind(facts.KindAssociation) {
		name, _ := fact.PropAny("association").(string)
		model, _ := fact.PropAny("model").(string)
		target, _ := fact.PropAny("target").(string)
		if name == "" || target == "" {
			continue
		}
		index.byName[name] = append(index.byName[name], target)
		if index.onType[model] == nil {
			index.onType[model] = map[string]string{}
		}
		index.onType[model][name] = target
	}
	return index
}
