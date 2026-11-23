package orphans

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/enola-labs/enola/internal/facts"
)

// Laravel, and the libraries written for it, call much of an application by
// convention: a migrator runs every migration file, the console kernel loads every
// command in app/Console/Commands, Eloquent calls a model's casts() and an observer's
// created(), a mailer calls a mailable's envelope(). None of it is a call the graph
// can record, so each read as dead code.
//
// Every rule here is keyed on the class-level fact that makes it true: the class
// extends, implements or uses the framework type (an `implements` edge on its symbol,
// followed through the application's own classes), and for a class that is itself
// loaded, the directory the framework loads it from. A method name alone roots
// nothing, so a service's own created() or handle() is still reported.

const (
	laravelModel     = `Illuminate\Database\Eloquent\Model`
	laravelMigration = `Illuminate\Database\Migrations\Migration`
	laravelCommand   = `Illuminate\Console\Command`
	laravelSeeder    = `Illuminate\Database\Seeder`
	laravelManager   = `Illuminate\Support\Manager`
	laravelNotice    = `Illuminate\Notifications\Notification`
	fractalTransform = `League\Fractal\TransformerAbstract`
	serviceProvider  = `Illuminate\Support\ServiceProvider`
	propFractalIncl  = "fractal_includes"
)

// laravelExternalParents are framework types whose own parents the store never
// declares, because the framework is not part of the application.
var laravelExternalParents = map[string][]string{
	`Laravel\Pulse\Support\PulseMigration`:                         {laravelMigration},
	`Laravel\Ai\Migrations\AiMigration`:                            {laravelMigration},
	`Illuminate\Foundation\Auth\User`:                              {laravelModel},
	`Illuminate\Database\Eloquent\Relations\Pivot`:                 {laravelModel},
	`Illuminate\Database\Eloquent\Relations\MorphPivot`:            {`Illuminate\Database\Eloquent\Relations\Pivot`},
	`Illuminate\Notifications\DatabaseNotification`:                {laravelModel},
	`Illuminate\Contracts\Broadcasting\ShouldBroadcastNow`:         {`Illuminate\Contracts\Broadcasting\ShouldBroadcast`},
	`Illuminate\Contracts\Queue\ShouldBeUniqueUntilProcessing`:     {`Illuminate\Contracts\Queue\ShouldBeUnique`},
	`Illuminate\Foundation\Support\Providers\EventServiceProvider`: {serviceProvider},
	`Illuminate\Foundation\Support\Providers\RouteServiceProvider`: {serviceProvider},
	`Illuminate\Foundation\Support\Providers\AuthServiceProvider`:  {serviceProvider},
	`Laravel\Horizon\HorizonApplicationServiceProvider`:            {serviceProvider},
	`Laravel\Telescope\TelescopeApplicationServiceProvider`:        {serviceProvider},
	`Illuminate\Foundation\Auth\AuthenticatesUsers`:                {`Illuminate\Foundation\Auth\RedirectsUsers`, `Illuminate\Foundation\Auth\ThrottlesLogins`},
	`Illuminate\Foundation\Auth\RegistersUsers`:                    {`Illuminate\Foundation\Auth\RedirectsUsers`},
	`Illuminate\Foundation\Auth\ResetsPasswords`:                   {`Illuminate\Foundation\Auth\RedirectsUsers`},
	`Illuminate\Foundation\Auth\VerifiesEmails`:                    {`Illuminate\Foundation\Auth\RedirectsUsers`},
	`Illuminate\Foundation\Auth\ConfirmsPasswords`:                 {`Illuminate\Foundation\Auth\RedirectsUsers`},
}

func methodSet(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// laravelHooks names, per framework type, the methods the framework calls on a class
// that extends, implements or uses it.
var laravelHooks = map[string]map[string]bool{
	laravelModel: methodSet("casts", "boot", "booted", "booting", "newFactory", "newCollection",
		"newEloquentBuilder", "getRouteKeyName", "resolveRouteBinding"),
	`Illuminate\Database\Eloquent\Factories\HasFactory`: methodSet("newFactory"),
	`Illuminate\Database\Eloquent\Factories\Factory`:    methodSet("definition", "configure"),
	laravelSeeder:    methodSet("run"),
	laravelMigration: methodSet("up", "down", "getConnection", "shouldRun"),
	laravelCommand:   methodSet("handle", "promptForMissingArgumentsUsing", "afterPromptingForMissingArguments"),
	serviceProvider:  methodSet("register", "boot", "provides", "when"),
	`Illuminate\Foundation\Support\Providers\EventServiceProvider`: methodSet("shouldDiscoverEvents", "discoverEventsWithin"),
	`Illuminate\Foundation\Support\Providers\RouteServiceProvider`: methodSet("map"),
	`Laravel\Horizon\HorizonApplicationServiceProvider`:            methodSet("gate"),
	`Laravel\Telescope\TelescopeApplicationServiceProvider`:        methodSet("gate"),
	`Illuminate\Foundation\Console\Kernel`:                         methodSet("schedule", "commands", "scheduleTimezone"),
	`Illuminate\Foundation\Exceptions\Handler`:                     methodSet("register", "report", "render", "context"),
	`Illuminate\Foundation\Http\FormRequest`: methodSet("authorize", "rules", "messages", "attributes",
		"prepareForValidation", "passedValidation", "failedValidation", "failedAuthorization",
		"withValidator", "after", "validationData"),
	`Illuminate\Mail\Mailable`:          methodSet("build", "envelope", "content", "attachments", "headers"),
	laravelNotice:                       methodSet("via", "shouldSend", "viaQueues", "viaConnections", "withDelay"),
	`Illuminate\Support\Facades\Facade`: methodSet("getFacadeAccessor"),
	laravelManager:                      methodSet("getDefaultDriver"),
	`Illuminate\Contracts\Broadcasting\ShouldBroadcast`: methodSet("broadcastOn", "broadcastAs", "broadcastWith",
		"broadcastWhen", "broadcastQueue", "broadcastConnections"),
	`Illuminate\Contracts\Queue\ShouldQueue`: methodSet("handle", "failed", "retryUntil", "backoff", "middleware",
		"tags", "displayName", "uniqueId", "uniqueFor", "uniqueVia", "viaQueue", "viaConnection", "shouldQueue"),
	`Illuminate\Contracts\Queue\ShouldBeUnique`:      methodSet("uniqueId", "uniqueFor", "uniqueVia"),
	`Illuminate\Foundation\Bus\Dispatchable`:         methodSet("handle", "failed", "middleware"),
	`Illuminate\Contracts\Validation\ValidationRule`: methodSet("validate"),
	`Illuminate\Contracts\Validation\Rule`:           methodSet("passes", "message"),
	`Illuminate\Foundation\Auth\RedirectsUsers`:      methodSet("redirectTo"),
	`Illuminate\Foundation\Auth\ThrottlesLogins`:     methodSet("throttleKey", "maxAttempts", "decayMinutes"),
	`Illuminate\Foundation\Auth\AuthenticatesUsers`: methodSet("authenticated", "username", "credentials",
		"validateLogin", "attemptLogin", "sendLoginResponse", "sendFailedLoginResponse", "loggedOut", "guard"),
	`Illuminate\Foundation\Auth\RegistersUsers`: methodSet("registered", "guard"),
	`Illuminate\Foundation\Auth\SendsPasswordResetEmails`: methodSet("validateEmail", "credentials",
		"sendResetLinkResponse", "sendResetLinkFailedResponse", "broker"),
	`Illuminate\Foundation\Auth\ResetsPasswords`: methodSet("rules", "validationErrorMessages", "credentials",
		"resetPassword", "setUserPassword", "sendResetResponse", "sendResetFailedResponse", "broker", "guard"),
	`Illuminate\Foundation\Auth\ConfirmsPasswords`: methodSet("rules", "validationErrorMessages"),
	`Laravel\Scout\Searchable`: methodSet("toSearchableArray", "searchableAs", "shouldBeSearchable",
		"searchIndexShouldBeUpdated", "makeSearchableUsing", "makeAllSearchableUsing", "getScoutKey", "getScoutKeyName"),
	fractalTransform: methodSet("transform"),
}

// eloquentEvents are the model events an observer method is called for, by name.
var eloquentEvents = methodSet("retrieved", "creating", "created", "updating", "updated", "saving", "saved",
	"deleting", "deleted", "trashed", "forceDeleting", "forceDeleted", "restoring", "restored", "replicating")

// phpType is a PHP class, interface, trait or enum as the store declares it.
type phpType struct {
	file     string
	abstract bool
	parents  []string
	names    []string
	includes []string
}

// laravelIndex is what the rules read: the application's types and the framework
// types each one reaches.
type laravelIndex struct {
	types     map[string]*phpType
	ancestors map[string]map[string]bool
}

func (x *laravelIndex) is(class, base string) bool {
	return x.ancestorsOf(class)[base]
}

// ancestorsOf is every type class extends, implements or uses, through the
// application's own types and the framework's known ones.
func (x *laravelIndex) ancestorsOf(class string) map[string]bool {
	if a, ok := x.ancestors[class]; ok {
		return a
	}
	out := map[string]bool{}
	x.ancestors[class] = out // a cycle in broken code ends here
	stack := []string{class}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		var parents []string
		if t := x.types[cur]; t != nil {
			parents = t.parents
		} else {
			parents = laravelExternalParents[cur]
		}
		for _, p := range parents {
			if !out[p] && p != class {
				out[p] = true
				stack = append(stack, p)
			}
		}
	}
	return out
}

// laravelRoots returns the PHP symbols a Laravel-family framework invokes by
// convention, and for each Eloquent local scope the names a call to it is written
// under (`scopeActive` is called as `active`).
func laravelRoots(symbols []facts.Fact, fileRefs []facts.Fact) (map[string]bool, map[string][]string) {
	x := &laravelIndex{types: map[string]*phpType{}, ancestors: map[string]map[string]bool{}}
	for _, f := range symbols {
		if f.PropString(propLanguage) != "php" {
			continue
		}
		switch f.PropString(propSymbolKind) {
		case facts.SymbolClass, facts.SymbolInterface, facts.SymbolEnum:
		default:
			continue
		}
		t := x.types[f.Name]
		if t == nil {
			t = &phpType{file: f.File}
			x.types[f.Name] = t
		}
		for _, r := range f.Relations {
			switch r.Kind {
			case facts.RelImplements:
				t.parents = append(t.parents, r.Target)
			case facts.RelNames:
				t.names = append(t.names, r.Target)
			}
		}
		if f.PropBool("abstract") {
			t.abstract = true
		}
		if raw, ok := f.Prop(propFractalIncl); ok {
			t.includes = append(t.includes, toStringSlice(raw)...)
		}
	}
	if len(x.types) == 0 {
		return nil, nil
	}

	roots := map[string]bool{}
	for name, t := range x.types {
		if t.abstract {
			continue // the loaders skip an abstract class; its subclasses are what they load
		}
		switch {
		case x.is(name, laravelMigration) && hasSegment(t.file, "migrations"):
			roots[name] = true
		case x.is(name, laravelCommand) && underDir(t.file, "app/Console/Commands"):
			roots[name] = true
		case x.is(name, laravelSeeder) && shortClassName(name) == "DatabaseSeeder":
			roots[name] = true
		}
	}

	for name := range x.discoveredPolicies() {
		roots[name] = true
	}
	for name := range x.flarumCommandHandlers(symbols, fileRefs) {
		roots[name] = true
	}

	observers := x.observers(symbols, fileRefs)
	modelTraits := x.modelTraits()
	scopes := map[string][]string{}
	for _, f := range symbols {
		if f.PropString(propLanguage) != "php" || f.PropString(propSymbolKind) != facts.SymbolMethod {
			continue
		}
		i := strings.LastIndex(f.Name, "::")
		if i <= 0 {
			continue
		}
		owner, method := f.Name[:i], f.Name[i+2:]
		if _, ok := x.types[owner]; !ok {
			continue
		}
		if x.hooked(owner, method) || (observers[owner] && eloquentEvents[method]) || (modelTraits[owner] && traitBoots(owner, method)) {
			roots[f.Name] = true
			continue
		}
		if alias, ok := strings.CutPrefix(method, "scope"); ok && upperFirst(alias) && x.is(owner, laravelModel) {
			scopes[f.Name] = x.scopeCalls(owner, lowerFirst(alias))
		}
	}
	return roots, scopes
}

// hooked reports whether the framework a class is built on calls method on it.
func (x *laravelIndex) hooked(class, method string) bool {
	ancestors := x.ancestorsOf(class)
	for base := range ancestors {
		if laravelHooks[base][method] {
			return true
		}
	}
	switch {
	case ancestors[laravelNotice]:
		// A channel calls to<Channel>: toMail, toDatabase, a package channel's toSlack.
		rest, ok := strings.CutPrefix(method, "to")
		return ok && upperFirst(rest)
	case ancestors[laravelManager]:
		// A manager builds the driver named `x` by calling createXDriver.
		mid, ok := strings.CutPrefix(method, "create")
		return ok && strings.HasSuffix(mid, "Driver") && upperFirst(mid)
	case ancestors[fractalTransform]:
		return x.includes(class)[method]
	}
	return false
}

// includes is the set of include methods Fractal calls on a transformer: one per
// entry its include lists, or a parent's, hold.
func (x *laravelIndex) includes(class string) map[string]bool {
	out := map[string]bool{}
	for _, c := range append([]string{class}, sortedKeys(x.ancestorsOf(class))...) {
		t := x.types[c]
		if t == nil {
			continue
		}
		for _, entry := range t.includes {
			entry, _, _ = strings.Cut(entry, ".") // a nested include starts at this transformer
			out["include"+studly(entry)] = true
		}
	}
	return out
}

// modelTraits are the application's own types an Eloquent model is built on. A model
// boots each trait it uses by calling the trait's boot<Trait> and initialize<Trait>.
func (x *laravelIndex) modelTraits() map[string]bool {
	out := map[string]bool{}
	for name := range x.types {
		if !x.is(name, laravelModel) {
			continue
		}
		for a := range x.ancestorsOf(name) {
			if x.types[a] != nil {
				out[a] = true
			}
		}
	}
	return out
}

func traitBoots(trait, method string) bool {
	short := shortClassName(trait)
	return method == "boot"+short || method == "initialize"+short
}

// observers are the classes registered as an Eloquent observer: named by a body that
// calls observe() on a model (`Order::observe(OrderObserver::class)`), or by a model
// class itself (`#[ObservedBy(OrderObserver::class)]`).
func (x *laravelIndex) observers(symbols, fileRefs []facts.Fact) map[string]bool {
	out := map[string]bool{}
	registers := func(f facts.Fact) bool {
		for _, r := range f.Relations {
			if r.Kind != facts.RelCalls {
				continue
			}
			if recv, ok := strings.CutSuffix(r.Target, "::observe"); ok && x.is(recv, laravelModel) {
				return true
			}
		}
		return false
	}
	for _, list := range [][]facts.Fact{symbols, fileRefs} {
		for _, f := range list {
			if f.PropString(propLanguage) != "php" {
				continue
			}
			model := f.Kind == facts.KindSymbol && x.types[f.Name] != nil && x.is(f.Name, laravelModel)
			if !model && !registers(f) {
				continue
			}
			for _, r := range f.Relations {
				if r.Kind == facts.RelNames {
					out[r.Target] = true
				}
			}
		}
	}
	return out
}

// scopeCalls is the names a call to a model's scope is recorded under: the bare
// method, and the method on the model or any model built on it.
func (x *laravelIndex) scopeCalls(model, alias string) []string {
	out := []string{alias}
	for name := range x.types {
		if name == model || x.is(name, model) {
			out = append(out, name+"::"+alias)
		}
	}
	sort.Strings(out)
	return out
}

// creditScopes marks each scope used by whoever calls it under its alias.
func creditScopes(scopes map[string][]string, refSources refIndex) {
	names := make([]string, 0, len(scopes))
	for name := range scopes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, scope := range names {
		for _, call := range scopes[scope] {
			s := refSources[call]
			if s == nil {
				continue
			}
			if s.prodN > 0 {
				refSources.add(scope, s.prodOne)
				for _, src := range s.fold {
					refSources.add(scope, src)
				}
			}
			for _, src := range s.testFold {
				refSources.addTest(scope, src)
			}
		}
	}
}

func hasSegment(file, segment string) bool {
	for _, part := range strings.Split(file, "/") {
		if part == segment {
			return true
		}
	}
	return false
}

func underDir(file, dir string) bool {
	return strings.HasPrefix(file, dir+"/") || strings.Contains(file, "/"+dir+"/")
}

func shortClassName(name string) string {
	return name[strings.LastIndex(name, `\`)+1:]
}

func upperFirst(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return r != utf8.RuneError && unicode.IsUpper(r)
}

func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToLower(r)) + s[n:]
}

// studly is Fractal's spelling of an include name as a method suffix:
// `ordered_items` and `orderedItems` are both OrderedItems.
func studly(s string) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' || r == ' ' }) {
		r, n := utf8.DecodeRuneInString(part)
		b.WriteRune(unicode.ToUpper(r))
		b.WriteString(part[n:])
	}
	return b.String()
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
