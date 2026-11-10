package flows

import (
	"slices"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/hierarchy"
)

// Interface dispatch.
//
// A call through an interface resolves to the interface's method, which has no body:
// `_sessionManager.AuthenticateNewSession(...)` in a controller names
// ISessionManager.AuthenticateNewSession, and a walk that follows only calls stops
// there. What runs is the method of the class implementing it. In an application
// wired by dependency injection that is nearly always one class, so the walk goes on
// into the implementations of an interface method, when there are few enough of them
// for the flow to stay a description of one path. Jellyfin has 1,867 calls to an
// interface with one implementation, and 8 to one with more than three.

// The same holds for an abstract class, and for an abstract method of any class: what
// runs is a subclass's method. A concrete method of a concrete class is not followed
// into its overrides, since a call resolved to a body says what runs, as in ioclosure.

// maxImplementations is the most implementations an interface method is followed
// into. Past it the interface is an extension point, a logger or a storage backend,
// and every implementation in the flow would describe none of them.
const maxImplementations = 3

// Self types.
//
// A method runs on an object, and what a call to another member of that object runs
// depends on the object's type, not on where the method was declared. Base.run calls
// self.step(); walked from a Concrete it runs Concrete.step, and the graph's edge
// says Base.step, often an abstract method with no body. So the walk carries a self
// type, the type of the object the code it is in runs on:
//
//   - entering a member of T, it is T;
//   - a call the extractor spelled through a subtype S (inherited_calls: c.run() on
//     a Concrete, resolved to Base.run) makes it S;
//   - inside a member of S or of one of its ancestors, a call to a member of S or of
//     one of its ancestors is a call on the same object: it goes to the declaration
//     the member name reaches from S, which is S's override when S has one, and the
//     self type stays S.
//
// With no self type known, an abstract method still goes on into its implementations
// as below. The rule reads `other.step()` on another object of an ancestor type as a
// call on self; the walk then follows the override of the type it came through, which
// is one of the methods that call could run.

// maxContexts is how many self types one symbol is walked in before the walk goes on
// without one. A base method every subclass of a wide tree reaches is otherwise
// walked once per subclass.
const maxContexts = 4

// memberCall is a call to a member of a type, kept with what a self type needs to
// redirect it.
type memberCall struct {
	to     int32 // the node the call names, or -1 when it names no symbol
	owner  string
	member string
	via    []string // the subtypes the call was spelled through (inherited_calls)
}

// step is where a walk goes from a symbol: a callee and the self type it runs in,
// interned (selfID), 0 for none.
type step struct {
	id   int32
	self int32
}

// selfID interns a self type, so the walk's visited set is keyed by two ints.
func (a *Analysis) selfID(name string) int32 {
	if name == "" {
		return 0
	}
	if id, ok := a.selfIDs[name]; ok {
		return id
	}
	if a.selfIDs == nil {
		a.selfIDs = map[string]int32{}
		a.selves = []string{""}
	}
	id := int32(len(a.selves))
	a.selves = append(a.selves, name)
	a.selfIDs[name] = id
	return id
}

// ownerType returns the type a symbol is a member of, or "".
func (a *Analysis) ownerType(f facts.Fact) string {
	switch f.PropString("symbol_kind") {
	case facts.SymbolMethod, facts.SymbolGetter, facts.SymbolFunc:
	default:
		return ""
	}
	owner, _ := hierarchy.SplitMember(f.Name)
	if owner == "" || !a.types().IsType(f.Repo, owner) {
		return ""
	}
	return owner
}

// inheritedThrough maps each declaration a symbol's inherited calls resolved to onto
// the subtypes they were spelled through.
func (a *Analysis) inheritedThrough(f facts.Fact) map[string][]string {
	spelled := f.PropStrings(facts.PropInheritedCalls)
	if len(spelled) == 0 {
		return nil
	}
	out := map[string][]string{}
	for _, s := range spelled {
		owner, member := hierarchy.SplitMember(s)
		if decl, ok := a.types().Lookup(f.Repo, owner, member); ok && !slices.Contains(out[decl], owner) {
			out[decl] = append(out[decl], owner)
		}
	}
	return out
}

// addMemberCall records a call when its target is a member of a type.
func (a *Analysis) addMemberCall(list []memberCall, repo, target string, to int32, resolved bool, through map[string][]string) []memberCall {
	owner, member := hierarchy.SplitMember(target)
	if owner == "" || !a.types().IsType(repo, owner) {
		return list
	}
	if !resolved {
		to = -1
	}
	for _, m := range list {
		if m.owner == owner && m.member == member {
			return list
		}
	}
	return append(list, memberCall{to: to, owner: owner, member: member, via: through[target]})
}

// successors returns where the walk goes from a symbol run in a self type: its
// callees, with every call on the same object sent to the declaration the self type
// reaches, and each callee's own self type. The answer depends only on the pair, and
// every flow's walk asks again, so it is kept.
func (a *Analysis) successors(id, self int32) []step {
	at := step{id, self}
	if out, ok := a.succ[at]; ok {
		return out
	}
	a.expand(id)
	// nodeFor below may grow a.nodes, so nothing holds a pointer into it.
	n := a.nodes[id]
	repo := n.fact.Repo
	selfName := ""
	if self != 0 {
		selfName = a.selves[self]
	}
	var out []step
	var redirected map[int32]bool
	if len(n.members) > 0 {
		t := a.types()
		inside := selfName != "" && n.owner != "" && t.Subsumes(repo, n.owner, selfName)
		for _, m := range n.members {
			on := m.via
			if len(on) == 0 && inside && t.Subsumes(repo, m.owner, selfName) {
				on = []string{selfName}
			}
			for _, typ := range on {
				decl, ok := t.Lookup(repo, typ, m.member)
				if !ok {
					continue
				}
				to, ok := a.nodeFor(repo, decl)
				if !ok || to == id {
					continue
				}
				if m.to >= 0 {
					if redirected == nil {
						redirected = map[int32]bool{}
					}
					redirected[m.to] = true
				}
				out = appendStep(out, step{to, a.selfID(typ)})
			}
		}
	}
	for _, c := range n.callees {
		if !redirected[c] {
			out = appendStep(out, step{c, a.nodes[c].ownerID})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].id != out[j].id {
			return a.less(out[i].id, out[j].id)
		}
		return out[i].self < out[j].self
	})
	if a.succ == nil {
		a.succ = map[step][]step{}
	}
	a.succ[at] = out
	return out
}

func appendStep(list []step, s step) []step {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// types is the repository's type tree, built the first time the walk needs it.
func (a *Analysis) types() *hierarchy.Index {
	if a.tree == nil {
		a.tree = hierarchy.Build(a.src.FactsRef())
	}
	return a.tree
}

// implementations returns the nodes of the methods implementing an interface or
// abstract method, or nil when the node is not one, or has none or too many. They
// are the topmost overriders below the method's type, however deep the first one
// down a branch sits.
func (a *Analysis) implementations(id int32) []int32 {
	f := a.nodes[id].fact
	if f.PropString("symbol_kind") != facts.SymbolMethod {
		return nil
	}
	owner, method := hierarchy.SplitMember(f.Name)
	t := a.types()
	if owner == "" || !t.IsType(f.Repo, owner) {
		return nil
	}
	if !t.IsAbstract(f.Repo, owner) && !f.PropBool("abstract") {
		return nil
	}
	impls := t.Overriders(f.Repo, owner, method)
	if len(impls) == 0 || len(impls) > maxImplementations {
		return nil
	}
	var out []int32
	for _, name := range impls {
		if impl, ok := a.nodeFor(f.Repo, name); ok && impl != id {
			out = append(out, impl)
		}
	}
	return out
}

// Classes.
//
// A class has no calls of its own; its methods do. A class reached as an entry, a
// route served by a Flutter page widget for one, is entered through its methods. A
// Flutter widget or state class is entered through them wherever it is reached,
// because the framework, not the code, calls them: `build` when the widget is put on
// screen, and `createState` for a stateful one, whose state class builds in turn. One
// widget reaches another by constructing it, so for these classes `instantiates` is
// followed as well as `calls`.

// flutterRoles are the classes whose methods the Flutter framework calls.
var flutterRoles = map[string]bool{"widget": true, "widget_state": true}

// classMembers returns the nodes a class node continues into: its methods, when it is
// an entry or a Flutter class, and nothing for anything else.
func (a *Analysis) classMembers(id int32) []int32 {
	f := a.nodes[id].fact
	if k := f.PropString("symbol_kind"); k != facts.SymbolClass && k != facts.SymbolStruct {
		return nil
	}
	if !a.entries[id] && !flutterRoles[f.PropString("flutter_role")] {
		return nil
	}
	var out []int32
	for _, m := range a.types().Members(f.Repo, f.Name) {
		if n, ok := a.nodeFor(f.Repo, m); ok && n != id {
			out = append(out, n)
		}
	}
	return out
}

// isFlutterClass reports whether a node is a class the Flutter framework drives.
func (a *Analysis) isFlutterClass(id int32) bool {
	f := a.nodes[id].fact
	return f.PropString("symbol_kind") == facts.SymbolClass && flutterRoles[f.PropString("flutter_role")]
}

// Apps a platform starts.
//
// An Android app, an iOS app and a WinUI or Avalonia app have no main: the platform
// constructs a type and calls its lifecycle methods. That type is recognized by what
// it implements, per language, and is an entry walked through its methods.

// appBases are the types whose implementers a platform starts, by language.
var appBases = map[string]map[string]bool{
	// Android's Service is left out: `Service` is the name of half the interfaces of
	// any JVM codebase, Hive's among them.
	"kotlin": {"Activity": true, "ComponentActivity": true, "AppCompatActivity": true, "FragmentActivity": true,
		"Application": true, "BroadcastReceiver": true},
	"java": {"Activity": true, "ComponentActivity": true, "AppCompatActivity": true, "FragmentActivity": true,
		"Application": true, "BroadcastReceiver": true},
	"swift":  {"App": true, "UIApplicationDelegate": true, "NSApplicationDelegate": true},
	"csharp": {"Application": true},
	// A XAML application's class is declared by its App.xaml as well as its code.
	"xaml": {"Application": true},
}

// startsApp reports whether a platform starts a type: it implements one of its
// language's application or activity types.
func startsApp(f facts.Fact) bool {
	bases := appBases[f.PropString("language")]
	if bases == nil {
		return false
	}
	for _, r := range f.Relations {
		if r.Kind == facts.RelImplements && bases[r.Target[strings.LastIndexByte(r.Target, '.')+1:]] {
			return true
		}
	}
	return false
}
