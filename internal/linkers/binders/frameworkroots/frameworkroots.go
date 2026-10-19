// Package frameworkroots marks where a framework enters application code, and
// what that entry reaches.
//
// The graph records who calls whom inside the application and nothing about
// the calls a framework makes into it: a router dispatching to an action, a
// queue draining a job, a mailer delivering, a migration running, a class
// body registering one of its own methods as a callback. Without those
// roots every one of them looks unreachable, which is why a dead-code
// verdict was declined twice on the same measurement.
//
// The first pass sets RootProp on the facts that already exist, with the
// mechanism as the value: a route and the action it resolves to, and the
// methods a framework invokes on a class the store can tie to that
// framework (its component, its superclass, or a mixin it declares). The
// table is data and every row names the class-level evidence it requires;
// a method name alone roots nothing, so a service object's call is not a
// root and a job's perform is.
//
// A library is not the only thing that calls into a class by convention. A
// mixin whose `included` hook hands the including class to another constant
// (`Registry.register(base)`) enrols every includer in a registry, and the
// registry later calls the members the mixin declares as its contract. The
// same holds for a base class whose `inherited` hook does it for each
// subclass. Those members are roots with the mechanism registry: the hook's
// call is the class-level evidence, and the contract is read off the
// registering module itself rather than off a list of names.
//
// The second pass walks calls, handled_by and depends_on edges from every
// root and sets ReachedFromProp on each symbol reached, naming the
// mechanisms that reach it. A bare call name resolves only against the
// caller's own class, which is what Ruby does with an implicit receiver;
// anything else the walk cannot follow stays unreached and is counted, so a
// reader can tell a dead symbol from one behind an untyped receiver.
//
// Both passes live in one binder because the second depends on the first,
// and binders may not depend on the order they were registered in.
package frameworkroots

import (
	"context"
	"log"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// RootProp names the mechanism through which a framework invokes a symbol:
// route, job, mailer, hook, task or callback. A route fact carries it too.
const RootProp = facts.PropFrameworkRoot

// ReachedFromProp lists, comma-separated and sorted, the root mechanisms
// whose walk reaches a symbol that is not itself a root.
const ReachedFromProp = "reached_from"

const (
	MechanismRoute    = "route"
	MechanismJob      = "job"
	MechanismMailer   = "mailer"
	MechanismHook     = "hook"
	MechanismTask     = "task"
	MechanismCallback = "callback"
	MechanismRegistry = "registry"
)

var mechanisms = []string{MechanismRoute, MechanismJob, MechanismMailer, MechanismHook, MechanismTask, MechanismCallback, MechanismRegistry}

// mixinRegistrationHooks are the singleton hooks Ruby runs when a module is
// mixed into a class; inheritRegistrationHooks the one it runs when a class is
// subclassed. Each receives the class, which is what a registering hook hands on.
var mixinRegistrationHooks = map[string]bool{"included": true, "extended": true, "prepended": true}

var inheritRegistrationHooks = map[string]bool{"inherited": true}

var jobMixins = map[string]bool{"Sidekiq::Worker": true, "Sidekiq::Job": true}

var jobSuperclasses = map[string]bool{"ApplicationJob": true, "ActiveJob::Base": true}

// mixinHooks names, per mixin, the methods the mixed-in library calls on the
// class that included it. A row applies only to a class whose mixin fact names
// the module, so the evidence is the include itself.
var mixinHooks = map[string]map[string]bool{
	"Sidekiq::Job::Iterable": {"build_enumerator": true, "each_iteration": true, "on_start": true, "on_resume": true, "on_stop": true, "on_complete": true, "on_cancel": true, "around_iteration": true},
	"Pundit::Authorization":  {"pundit_user": true, "pundit_params_for": true, "policy_scope": true, "authorize": true},
	"Pundit":                 {"pundit_user": true, "pundit_params_for": true, "policy_scope": true, "authorize": true},
}

var walkedRelations = map[string]bool{facts.RelCalls: true, facts.RelHandledBy: true, facts.RelDependsOn: true}

// superclassHooks names, per library base class, the methods the library
// calls on a subclass. A row applies through the superclass chain.
var superclassHooks = map[string]map[string]bool{
	"Scimitar::ResourcesController":                   {"storage_class": true, "storage_scope": true},
	"Scimitar::ActiveRecordBackedResourcesController": {"storage_class": true, "storage_scope": true},
}

type class struct {
	name       string
	component  string
	superclass string
	mixins     map[string]bool
	names      map[string]bool
	classes    map[string]*class
}

// ancestry is the class and its superclasses as far as the store declares
// them; what a superclass includes or is, the subclass inherits.
func (c *class) ancestry() []*class {
	out := []*class{c}
	seen := map[string]bool{c.name: true}
	for cur := c; cur.superclass != "" && !seen[cur.superclass]; {
		seen[cur.superclass] = true
		next := c.classes[cur.superclass]
		if next == nil {
			break
		}
		out = append(out, next)
		cur = next
	}
	return out
}

func (c *class) frameworkBound() bool {
	return c.component != "" || c.superclass != "" || len(c.mixins) > 0
}

func (c *class) hookedBy(method string) bool {
	for _, a := range c.ancestry() {
		if superclassHooks[a.superclass][method] {
			return true
		}
		for mixin := range a.mixins {
			if mixinHooks[mixin][method] {
				return true
			}
		}
	}
	return false
}

func (c *class) runsJobs() bool {
	for _, a := range c.ancestry() {
		if a.component == "job" || jobSuperclasses[a.superclass] {
			return true
		}
		for mixin := range a.mixins {
			if jobMixins[mixin] {
				return true
			}
		}
	}
	return false
}

type Binder struct{}

func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "framework-roots" }

func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	symbols := map[string]facts.Fact{}
	classes := map[string]*class{}
	for _, f := range store.ByKind(facts.KindSymbol) {
		if f.PropString("language") != "ruby" {
			continue
		}
		symbols[f.Name] = f
		switch f.PropString("symbol_kind") {
		case facts.SymbolClass, facts.SymbolInterface:
			c := &class{name: f.Name, component: f.PropString("rails_component"), superclass: f.PropString("superclass"), mixins: map[string]bool{}, names: map[string]bool{}, classes: classes}
			for _, r := range f.Relations {
				if r.Kind == facts.RelNames {
					c.names[r.Target] = true
				}
			}
			classes[f.Name] = c
		}
	}
	if len(symbols) == 0 {
		return nil
	}
	resolveSuperclasses(classes)
	for _, dep := range store.ByKind(facts.KindDependency) {
		if dep.PropString("mixin_kind") == "" {
			continue
		}
		includer, module, ok := strings.Cut(dep.Name, " -> ")
		if c := classes[includer]; ok && c != nil {
			c.mixins[module] = true
		}
	}

	roots := map[string]string{}
	routeTargets := map[string]string{}
	unresolvedRoutes := 0
	for _, route := range store.ByKind(facts.KindRoute) {
		if route.PropString("framework") != "rails" {
			continue
		}
		target := actionFor(route.PropString("handler"))
		if target == "" {
			continue
		}
		if _, ok := symbols[target]; !ok {
			unresolvedRoutes++
			continue
		}
		roots[target] = MechanismRoute
		routeTargets[routeKey(route)] = target
	}
	for name, f := range symbols {
		if f.PropString("symbol_kind") != facts.SymbolMethod {
			continue
		}
		if _, already := roots[name]; already {
			continue
		}
		owner, method, singleton := splitMember(name)
		c := classes[owner]
		if c == nil {
			continue
		}
		if mechanism := mechanismFor(c, f, method, singleton); mechanism != "" {
			roots[name] = mechanism
		}
	}
	rootFrameworkLoaded(symbols, classes, roots)
	rootRegistryContracts(symbols, classes, roots)

	adjacency := edges(store, symbols)
	reached := map[string]map[string]bool{}
	for _, mechanism := range mechanisms {
		frontier := []string{}
		for name, m := range roots {
			if m == mechanism {
				frontier = append(frontier, name)
			}
		}
		seen := map[string]bool{}
		for len(frontier) > 0 {
			next := []string{}
			for _, name := range frontier {
				for _, target := range adjacency[name] {
					if seen[target] {
						continue
					}
					seen[target] = true
					if _, isRoot := roots[target]; !isRoot {
						if reached[target] == nil {
							reached[target] = map[string]bool{}
						}
						reached[target][mechanism] = true
					}
					next = append(next, target)
				}
			}
			frontier = next
		}
	}

	perMechanism := map[string]int{}
	store.UpdateWhere(func(f *facts.Fact) {
		switch f.Kind {
		case facts.KindRoute:
			target, ok := routeTargets[routeKey(*f)]
			if !ok {
				return
			}
			setProp(f, RootProp, MechanismRoute)
			if !f.HasRelation(facts.RelHandledBy, target) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: target})
			}
		case facts.KindSymbol:
			if mechanism, ok := roots[f.Name]; ok {
				setProp(f, RootProp, mechanism)
				perMechanism[mechanism]++
				return
			}
			if by, ok := reached[f.Name]; ok {
				setProp(f, ReachedFromProp, joinSorted(by))
			}
		}
	})

	unreached := 0
	for name, f := range symbols {
		if f.PropString("symbol_kind") != facts.SymbolMethod {
			continue
		}
		if _, isRoot := roots[name]; isRoot {
			continue
		}
		if _, ok := reached[name]; !ok {
			unreached++
		}
	}
	if len(roots) > 0 {
		log.Printf("[framework-roots] %d root(s) (route %d, job %d, mailer %d, hook %d, task %d, callback %d, registry %d); %d route handler(s) name no known action; %d symbol(s) reached, %d method(s) unreached",
			len(roots), perMechanism[MechanismRoute], perMechanism[MechanismJob], perMechanism[MechanismMailer], perMechanism[MechanismHook], perMechanism[MechanismTask], perMechanism[MechanismCallback], perMechanism[MechanismRegistry],
			unresolvedRoutes, len(reached), unreached)
	}
	return nil
}

func mechanismFor(c *class, f facts.Fact, method string, singleton bool) string {
	switch {
	case !singleton && method == "perform" && c.runsJobs():
		return MechanismJob
	case !singleton && c.component == "mailer" && f.PropBool("exported"):
		return MechanismMailer
	case !singleton && c.component == "channel" && (method == "subscribed" || method == "unsubscribed"):
		return MechanismHook
	case !singleton && c.hookedBy(method):
		return MechanismHook
	case !singleton && isMigration(f.File) && (method == "change" || method == "up" || method == "down"):
		return MechanismTask
	case c.frameworkBound() && c.names[method]:
		return MechanismCallback
	}
	return ""
}

// resolveSuperclasses qualifies a superclass written relative to the class's own
// namespace (`class PresenceValidator < Base` inside Grape::Validations::Validators)
// the way Ruby's lexical lookup does: the innermost enclosing namespace that
// declares it wins. A name the store declares as written, or one it declares
// nowhere (a library's class), is left as written.
func resolveSuperclasses(classes map[string]*class) {
	for _, c := range classes {
		super := strings.TrimPrefix(c.superclass, "::")
		if super == "" || classes[super] != nil {
			c.superclass = super
			continue
		}
		c.superclass = super
		parts := strings.Split(c.name, "::")
		for i := len(parts) - 1; i >= 1; i-- {
			if cand := strings.Join(parts[:i], "::") + "::" + super; classes[cand] != nil {
				c.superclass = cand
				break
			}
		}
	}
}

// frameworkLoadedBases are library base classes whose own inherited hook
// registers every subclass with the framework, which then loads or runs it: Rails
// collects railties, engines and generators this way. A subclass is a root without
// any call naming it. The value says whether its public instance methods are
// roots too: Thor invokes each one of a generator's, in order, as a step.
var frameworkLoadedBases = map[string]bool{
	"Rails::Railtie":               false,
	"Rails::Engine":                false,
	"Rails::Application":           false,
	"Rails::Generators::Base":      true,
	"Rails::Generators::NamedBase": true,
	"Rails::Generators::AppBase":   true,
	"Thor::Group":                  true,
}

// rootFrameworkLoaded roots every class whose superclass chain reaches a
// frameworkLoadedBases entry, and, where the entry says so, its public instance
// methods.
func rootFrameworkLoaded(symbols map[string]facts.Fact, classes map[string]*class, roots map[string]string) {
	steps := map[string]bool{}
	for name, c := range classes {
		for _, a := range c.ancestry() {
			withSteps, ok := frameworkLoadedBases[a.superclass]
			if !ok {
				continue
			}
			if _, already := roots[name]; !already {
				roots[name] = MechanismRegistry
			}
			if withSteps {
				steps[name] = true
			}
			break
		}
	}
	if len(steps) == 0 {
		return
	}
	for name, f := range symbols {
		if _, already := roots[name]; already {
			continue
		}
		owner, _, singleton := splitMember(name)
		if !singleton && steps[owner] && f.PropString("symbol_kind") == facts.SymbolMethod && f.PropBool("exported") {
			roots[name] = MechanismRegistry
		}
	}
}

// rootRegistryContracts roots, on every class a registering module or base
// class enrols, the members that module declares.
//
// A member is matched by name across both forms, `Owner.name` (a singleton
// method; the extractor may record it as a method or a function) and
// `Owner#name`. Ruby does not copy a module's own `def self.run` onto an
// includer, so a contract written that way is nominal: the module declares
// the name to say what the registry will call, and the includer must define
// it in whichever form the registry calls it on. Rooting only the matching
// form would demand that the two agree, which the source does not promise.
func rootRegistryContracts(symbols map[string]facts.Fact, classes map[string]*class, roots map[string]string) {
	mixinContracts := map[string]map[string]bool{}
	inheritContracts := map[string]map[string]bool{}
	members := map[string]map[string]bool{}
	for name, f := range symbols {
		owner, member, singleton := splitMember(name)
		if owner == "" {
			continue
		}
		if members[owner] == nil {
			members[owner] = map[string]bool{}
		}
		members[owner][member] = true
		if !singleton || !registersCaller(owner, f) {
			continue
		}
		switch {
		case mixinRegistrationHooks[member]:
			mixinContracts[owner] = map[string]bool{}
		case inheritRegistrationHooks[member]:
			inheritContracts[owner] = map[string]bool{}
		}
	}
	for _, contracts := range []map[string]map[string]bool{mixinContracts, inheritContracts} {
		for owner, contract := range contracts {
			for member := range members[owner] {
				if !mixinRegistrationHooks[member] && !inheritRegistrationHooks[member] {
					contract[member] = true
				}
			}
		}
	}
	if len(mixinContracts) == 0 && len(inheritContracts) == 0 {
		return
	}

	for name, f := range symbols {
		if _, already := roots[name]; already {
			continue
		}
		if kind := f.PropString("symbol_kind"); kind != facts.SymbolMethod && kind != facts.SymbolFunc {
			continue
		}
		owner, member, _ := splitMember(name)
		c := classes[owner]
		if c == nil || mixinContracts[owner] != nil || inheritContracts[owner] != nil {
			continue // the registering module's own declaration is the contract, not an implementation
		}
		if enrolledFor(c, member, mixinContracts, inheritContracts) {
			roots[name] = MechanismRegistry
		}
	}
}

// enrolledFor reports whether a registering module mixed into the class, or a
// registering base class above it, declares member as part of its contract.
func enrolledFor(c *class, member string, mixinContracts, inheritContracts map[string]map[string]bool) bool {
	for i, a := range c.ancestry() {
		for mixin := range a.mixins {
			if mixinContracts[mixin][member] {
				return true
			}
		}
		// A class is enrolled by its superclasses' inherited hooks, never by its
		// own: the hook runs when something subclasses the class that defines it.
		if i == 0 {
			continue
		}
		if inheritContracts[a.name][member] {
			return true
		}
	}
	if inheritContracts[c.superclass][member] {
		return true // the direct superclass, also when the store does not declare it as a class
	}
	return false
}

// registersCaller reports whether a hook body hands its argument to another
// constant: it calls a method whose receiver is a constant other than the
// hook's own module (`Registry.register(base)`). A call on a lower-case
// receiver (`base.extend(ClassMethods)`) configures the class instead and
// registers nothing.
func registersCaller(owner string, f facts.Fact) bool {
	for _, r := range f.Relations {
		if r.Kind != facts.RelCalls {
			continue
		}
		receiver, _, singleton := splitMember(r.Target)
		if !singleton || receiver == "" || receiver == owner {
			continue
		}
		if first := strings.TrimPrefix(receiver, "::"); first != "" && first[0] >= 'A' && first[0] <= 'Z' {
			return true
		}
	}
	return false
}

func isMigration(file string) bool {
	return strings.Contains(file, "db/migrate/")
}

// actionFor turns a Rails handler such as `admin/accounts#show` into the
// symbol the graph names, `Admin::AccountsController#show`.
func actionFor(handler string) string {
	path, action, ok := strings.Cut(handler, "#")
	if !ok || path == "" || action == "" || strings.Contains(path, "://") || strings.ContainsAny(path, " :") {
		return ""
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		parts := strings.Split(segment, "_")
		for j, part := range parts {
			if part != "" {
				parts[j] = strings.ToUpper(part[:1]) + part[1:]
			}
		}
		segments[i] = strings.Join(parts, "")
	}
	return strings.Join(segments, "::") + "Controller#" + action
}

func routeKey(f facts.Fact) string {
	return f.Repo + "\x00" + f.File + "\x00" + f.Name + "\x00" + f.PropString("method") + "\x00" + f.PropString("handler")
}

func splitMember(name string) (owner, method string, singleton bool) {
	if hash := strings.LastIndex(name, "#"); hash > 0 {
		return name[:hash], name[hash+1:], false
	}
	if dot := strings.LastIndex(name, "."); dot > 0 && !strings.Contains(name[dot:], "::") {
		return name[:dot], name[dot+1:], true
	}
	return "", "", false
}

// edges collects, per symbol, the symbols its walked relations resolve to. A
// target that is already a symbol name resolves as written; a bare name
// resolves against the caller's own class and nowhere else.
func edges(store *facts.Store, symbols map[string]facts.Fact) map[string][]string {
	out := map[string][]string{}
	add := func(source, target string) {
		if resolved := resolve(source, target, symbols); resolved != "" && resolved != source {
			out[source] = append(out[source], resolved)
		}
	}
	for _, f := range store.ByKind(facts.KindSymbol) {
		if _, ok := symbols[f.Name]; !ok {
			continue
		}
		for _, r := range f.Relations {
			if walkedRelations[r.Kind] {
				add(f.Name, r.Target)
			}
		}
	}
	for _, f := range store.ByKind(facts.KindDependency) {
		source, _, ok := strings.Cut(f.Name, " -> ")
		if !ok {
			continue
		}
		if _, prefixed, hasPrefix := strings.Cut(source, ": "); hasPrefix {
			source = prefixed
		}
		if _, known := symbols[source]; !known {
			continue
		}
		for _, r := range f.Relations {
			if walkedRelations[r.Kind] {
				add(source, r.Target)
			}
		}
	}
	return out
}

func resolve(source, target string, symbols map[string]facts.Fact) string {
	if _, ok := symbols[target]; ok {
		return target
	}
	if strings.ContainsAny(target, "#.:") {
		return ""
	}
	owner, _, _ := splitMember(source)
	if owner == "" {
		owner = source
	}
	for _, candidate := range []string{owner + "#" + target, owner + "." + target} {
		if _, ok := symbols[candidate]; ok {
			return candidate
		}
	}
	return ""
}

func setProp(f *facts.Fact, key, value string) {
	if f.Props == nil {
		f.Props = map[string]any{}
	}
	f.SetProp(key, value)
}

func joinSorted(set map[string]bool) string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
