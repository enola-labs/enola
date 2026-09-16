// Package deadmethods reports Ruby methods nothing in the graph names, and
// methods only specs name.
//
// It asks a narrower question than "is this reachable". Whole-program
// reachability needs receiver types the graph does not have, and the
// entry-points explainer refuses the dead-code verdict for that reason. This
// one does not follow calls; it looks a method's bare name up in every call
// edge the graph holds (symbol to symbol, template to method, spec to method,
// the callback, delegate, serializer and naming DSLs folded in, symbol-to-proc
// block arguments, and a method name passed as a symbol argument for something
// else to dispatch) and reports the names nothing uses, and the names only
// spec files use. A name-based index keeps a method alive whenever any
// same-named method anywhere is called, so the list under-reports dead code;
// what it does report is a candidate to delete, never a verdict, and the one
// blind spot it cannot close is a send whose name is built at runtime and
// matches no recorded prefix.
//
// It is scoped to surfaces whose callers the graph can see: services, jobs,
// workers, queries and lib. Models are left out: their public methods are the
// surface serializers, JSON:API resources and Ember read declaratively, often
// through attribute lists splatted from constants, and on the monolith that
// made two thirds of the "only specs call it" readings wrong. Controllers
// (routed by name), views, components, helpers and presenters (called from
// templates the graph reads only partly), serializers, JSON:API resources,
// GraphQL types, Avo and mailers (framework-dispatched by convention) are
// left out, as are one-off surfaces: tasks, seeds, development helpers,
// importers. A name that matches
// a dynamic-send prefix recorded anywhere, a framework entry name, a writer,
// an operator or a Ruby protocol method is never reported.
package deadmethods

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/binders/frameworkroots"
)

const ExplainerName = "dead-methods"

type Explainer struct{}

func New() *Explainer { return &Explainer{} }

func (e *Explainer) Name() string { return ExplainerName }

// surfaces name where a method lives; the finding carries it so a reader can
// weigh a services method against a controller one, and so precision can be
// measured per surface. An empty name is a surface the reading does not judge:
// one-off code, specs, serializers (attribute lists splatted from constants
// and reflection the graph follows only partly) and mailers (Devise and
// `.with` chains dispatch by convention).
var surfaces = []struct{ prefix, name string }{
	{"app/services/development/", ""}, {"lib/tasks/", ""}, {"lib/development", ""}, {"lib/imports/", ""},
	{"db/", ""}, {"spec/", ""}, {"test/", ""},
	{"app/services/", "service"}, {"app/jobs/", "job"}, {"app/workers/", "job"}, {"app/queries/", "query"},
	{"app/models/", "model"}, {"app/controllers/", "controller"}, {"app/helpers/", "helper"},
	{"app/presenters/", "presenter"}, {"app/components/", "component"}, {"app/graphql/", "graphql"},
	{"app/serializers/", ""}, {"app/resources/", "resource"}, {"app/avo/", "avo"},
	{"app/mailers/", ""}, {"app/policies/", "policy"}, {"app/mcp_public_tools/", "mcp"},
	{"lib/", "lib"},
}

func surfaceOf(path string) string {
	for _, s := range surfaces {
		if strings.HasPrefix(path, s.prefix) {
			return s.name
		}
	}
	return ""
}

// entryNames are methods a framework calls rather than the application.
var entryNames = map[string]bool{
	"perform": true, "call": true, "deliver": true, "change": true, "up": true, "down": true,
	"execute": true, "handle": true, "run": true, "fields": true, "filters": true, "actions": true,
	"before_render": true, "render?": true, "process": true, "collection": true, "task_enumerator": true,
}

// protocolNames are Ruby and Rails protocol methods that are called by the
// runtime or by generic code, never by name in application code.
var protocolNames = map[string]bool{
	"initialize": true, "to_s": true, "to_str": true, "to_h": true, "to_a": true, "to_json": true,
	"as_json": true, "to_param": true, "to_proc": true, "to_partial_path": true, "inspect": true,
	"hash": true, "eql?": true, "==": true, "<=>": true, "[]": true, "[]=": true, "each": true,
	"method_missing": true, "respond_to_missing?": true, "serializable_hash": true,
	"cache_key": true, "cache_version": true, "model_name": true, "persisted?": true,
	"inherited": true, "included": true, "extended": true, "prepended": true,
	"call_with": true, "validate": true,
}

type candidate struct {
	name    string
	file    string
	repo    string
	method  string
	surface string
	specs   []string
}

// routedActions names every controller action a route handler points at, in
// the symbol's own spelling (`Admin::AccountsController#show`); a routed
// action is called by the router, which the graph holds as a route, not a call.
func routedActions(store *facts.Store) map[string]bool {
	out := map[string]bool{}
	for _, fact := range store.ByKind(facts.KindRoute) {
		handler, _ := fact.Props["handler"].(string)
		controller, action, ok := strings.Cut(handler, "#")
		if !ok || controller == "" {
			continue
		}
		out[controllerClass(controller)+"#"+action] = true
	}
	return out
}

func controllerClass(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		parts := strings.Split(segment, "_")
		for j, part := range parts {
			if part == "" {
				continue
			}
			parts[j] = strings.ToUpper(part[:1]) + part[1:]
		}
		segments[i] = strings.Join(parts, "")
	}
	return strings.Join(segments, "::") + "Controller"
}

func candidates(store *facts.Store) (uncalled, testOnly []candidate) {
	callers := map[string]map[string]bool{}
	prefixes := []string{}
	addCaller := func(target, file string) {
		name := bareName(target)
		if name == "" {
			return
		}
		if callers[name] == nil {
			callers[name] = map[string]bool{}
		}
		callers[name][file] = true
	}
	reflected := map[string]bool{}
	for _, kind := range []string{facts.KindSymbol, facts.KindFileRef, facts.KindTestRef} {
		for _, fact := range store.ByKind(kind) {
			for _, rel := range fact.Relations {
				if rel.Kind == facts.RelCalls || rel.Kind == facts.RelNames {
					addCaller(rel.Target, fact.File)
					if class := reflectedClass(rel.Target); class != "" {
						reflected[class] = true
					}
				}
			}
			for _, p := range propStrings(fact.Props["dynamic_send_prefixes"]) {
				if p != "" {
					prefixes = append(prefixes, p)
				}
			}
		}
	}
	if len(callers) == 0 {
		return nil, nil
	}
	routed := routedActions(store)

	for _, fact := range store.ByKind(facts.KindSymbol) {
		if lang, _ := fact.Props["language"].(string); lang != "ruby" {
			continue
		}
		if kind, _ := fact.Props["symbol_kind"].(string); kind != "method" {
			continue
		}
		path := repoRelative(fact.File)
		surface := surfaceOf(path)
		if surface == "" {
			continue
		}
		if routed[fact.Name] || fact.PropString(frameworkroots.RootProp) != "" {
			continue
		}
		if reflected[demodulize(className(fact.Name))] {
			continue
		}
		if surface == "controller" || surface == "model" {
			// Public controller methods are routes and public model methods are
			// the surface every serializer, resource and template reads, often
			// through strings the graph cannot follow; only private ones are
			// reached by name inside the class, where the graph sees every caller.
			if exported, ok := fact.Props["exported"].(bool); !ok || exported {
				continue
			}
		}
		method := methodName(fact.Name)
		if surface == "policy" && strings.HasSuffix(method, "?") {
			// A Pundit predicate is dispatched by the action's name
			// (`authorize record` asks `<action>?`); no call names it.
			continue
		}
		if method == "" || entryNames[method] || protocolNames[method] ||
			strings.HasSuffix(method, "=") || strings.HasPrefix(method, "_") || !isWord(method) {
			continue
		}
		if matchesPrefix(method, prefixes) {
			continue
		}
		by := callers[method]
		var others, specs []string
		for file := range by {
			if isTestFile(repoRelative(file)) {
				specs = append(specs, file)
			} else {
				others = append(others, file)
			}
		}
		if len(others) > 0 {
			continue
		}
		sort.Strings(specs)
		c := candidate{name: fact.Name, file: fact.File, repo: fact.Repo, method: method, surface: surface, specs: specs}
		if len(specs) == 0 {
			if len(by) > 0 {
				continue
			}
			uncalled = append(uncalled, c)
		} else {
			testOnly = append(testOnly, c)
		}
	}
	sort.Slice(uncalled, func(i, j int) bool { return uncalled[i].name < uncalled[j].name })
	sort.Slice(testOnly, func(i, j int) bool { return testOnly[i].name < testOnly[j].name })
	return uncalled, testOnly
}

// ClaimedSymbols is the set of methods this explainer reports on, computed by the
// same pass that produces the findings themselves.
//
// It exists so a broader analyzer can stay silent where this one speaks. The
// orphans analyzer asks whole-graph reachability over every language; this one
// asks the narrower question it can answer on Ruby, and is scoped to the surfaces
// whose callers the graph can actually see. Where both can see a method, the
// scoped answer is the one worth reporting.
//
// Direct call rather than a stamped prop or a read of the other's insights, for
// the reason given on queryloops.ClaimedSymbols: runExplainers forbids an
// explainer's output depending on whether another ran, and a prop would make it
// depend on whether another is enabled.
func ClaimedSymbols(store *facts.Store) map[string]struct{} {
	uncalled, testOnly := candidates(store)
	claimed := make(map[string]struct{}, len(uncalled)+len(testOnly))
	for _, c := range uncalled {
		claimed[c.name] = struct{}{}
	}
	for _, c := range testOnly {
		claimed[c.name] = struct{}{}
	}
	return claimed
}

func (e *Explainer) Explain(_ context.Context, store *facts.Store) ([]facts.Insight, error) {
	uncalled, testOnly := candidates(store)

	out := make([]facts.Insight, 0, len(uncalled)+len(testOnly))
	for _, c := range testOnly {
		evidence := []facts.Evidence{{File: c.file, Symbol: c.name, Detail: "defined here, surface: " + c.surface}}
		for _, s := range c.specs {
			evidence = append(evidence, facts.Evidence{File: s, Detail: "the only caller is a spec"})
		}
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("%s is called only from specs", c.name),
			Description: fmt.Sprintf("%s is named by %d spec file(s) and by nothing else the graph holds: no symbol, template or DSL declaration calls %q outside spec/. "+
				"A method kept alive by its tests is a candidate to delete together with those tests. The call index is by bare name, so any same-named method anywhere would keep it alive; "+
				"what keeps it here is that nothing does. Verify before deleting: a send built at runtime that matches no recorded prefix is the one caller this cannot see.",
				c.name, len(c.specs), c.method),
			Confidence: 0.6,
			Evidence:   evidence,
			Actions:    []string{"grep the repository for the bare name once; if only the specs name it, delete the method and those specs together", "if it is called through a name built at runtime, leave it and add the prefix to a literal send"},
		})
	}
	for _, c := range uncalled {
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("%s has no caller the graph can see", c.name),
			Description: fmt.Sprintf("Nothing in the graph names %q: no symbol, template, spec or DSL declaration calls it, and the name matches no dynamic-send prefix. "+
				"The call index is by bare name, so any same-named method anywhere would keep it alive; what keeps it here is that nothing does. "+
				"A candidate to delete, after one grep: a send built at runtime that matches no recorded prefix is the one caller this cannot see.", c.method),
			Confidence: 0.5,
			Evidence:   []facts.Evidence{{File: c.file, Symbol: c.name, Detail: "defined here, named nowhere, surface: " + c.surface}},
			Actions:    []string{"grep the repository for the bare name once; if nothing names it, delete it", "if it is reached through metaprogramming, say so next to the definition so the next reader does not ask again"},
		})
	}
	return out, nil
}

// reflectedClass returns the class a call target reflects over
// (`CachedAttributes.instance_methods`, `Thing.public_instance_methods`,
// `Thing.methods`), or "" for any other target. Every method of such a class is
// referenced by whatever consumes the list.
func reflectedClass(target string) string {
	for _, suffix := range []string{".instance_methods", ".public_instance_methods", ".methods", ".public_methods"} {
		if strings.HasSuffix(target, suffix) {
			return demodulize(strings.TrimSuffix(target, suffix))
		}
	}
	return ""
}

func className(symbol string) string {
	if i := strings.LastIndexAny(symbol, "#."); i >= 0 {
		return symbol[:i]
	}
	return symbol
}

func demodulize(name string) string {
	if i := strings.LastIndex(name, "::"); i >= 0 {
		return name[i+2:]
	}
	return name
}

func bareName(target string) string {
	t := target
	if i := strings.LastIndex(t, "::"); i >= 0 {
		t = t[i+2:]
	}
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	if i := strings.LastIndex(t, "#"); i >= 0 {
		t = t[i+1:]
	}
	return t
}

func methodName(symbol string) string {
	if i := strings.LastIndex(symbol, "#"); i >= 0 {
		return symbol[i+1:]
	}
	if i := strings.LastIndex(symbol, "."); i >= 0 {
		return symbol[i+1:]
	}
	return ""
}

// repoRelative drops a union snapshot's repo-label prefix so the path starts at
// a Rails root when it has one.
func repoRelative(file string) string {
	path := file
	for !startsAtRoot(path) {
		i := strings.Index(path, "/")
		if i < 0 {
			return file
		}
		path = path[i+1:]
	}
	return path
}

func startsAtRoot(path string) bool {
	for _, root := range []string{"app/", "lib/", "db/", "spec/", "test/", "config/"} {
		if strings.HasPrefix(path, root) {
			return true
		}
	}
	return false
}

func isTestFile(path string) bool {
	return strings.HasPrefix(path, "spec/") || strings.HasPrefix(path, "test/")
}

func matchesPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func isWord(name string) bool {
	for _, r := range name {
		if !isNameRune(r) {
			return false
		}
	}
	return true
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

func isNameRune(r rune) bool {
	return r == '_' || r == '?' || r == '!' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}
