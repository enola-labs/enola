package flows

import (
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
