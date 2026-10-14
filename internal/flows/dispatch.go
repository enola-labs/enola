package flows

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
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

// maxImplementations is the most implementations an interface method is followed
// into. Past it the interface is an extension point, a logger or a storage backend,
// and every implementation in the flow would describe none of them.
const maxImplementations = 3

// dispatchIndex is what the walk knows about types, per repository.
type dispatchIndex struct {
	interfaces map[[2]string]bool     // (repo, name) of every interface
	implements map[[2]string][]string // (repo, interface) -> the types implementing it
	methods    map[[2]string][]string // (repo, type) -> its methods
}

func (a *Analysis) dispatch() *dispatchIndex {
	if a.ifaces != nil {
		return a.ifaces
	}
	ix := &dispatchIndex{interfaces: map[[2]string]bool{}, implements: map[[2]string][]string{}, methods: map[[2]string][]string{}}
	for _, f := range a.src.FactsRef() {
		if f.Kind != facts.KindSymbol {
			continue
		}
		switch f.PropString("symbol_kind") {
		case facts.SymbolInterface:
			ix.interfaces[[2]string{f.Repo, f.Name}] = true
		case facts.SymbolMethod:
			if dot := strings.LastIndexByte(f.Name, '.'); dot > 0 {
				key := [2]string{f.Repo, f.Name[:dot]}
				if !containsString(ix.methods[key], f.Name) {
					ix.methods[key] = append(ix.methods[key], f.Name)
				}
			}
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelImplements {
				continue
			}
			key := [2]string{f.Repo, r.Target}
			if !containsString(ix.implements[key], f.Name) {
				ix.implements[key] = append(ix.implements[key], f.Name)
			}
		}
	}
	a.ifaces = ix
	return ix
}

// implementations returns the nodes of the methods implementing an interface method,
// or nil when the node is not one, or has none or too many.
func (a *Analysis) implementations(id int32) []int32 {
	f := a.nodes[id].fact
	if f.PropString("symbol_kind") != facts.SymbolMethod {
		return nil
	}
	dot := strings.LastIndexByte(f.Name, '.')
	if dot <= 0 {
		return nil
	}
	owner, method := f.Name[:dot], f.Name[dot+1:]
	ix := a.dispatch()
	if !ix.interfaces[[2]string{f.Repo, owner}] {
		return nil
	}
	types := ix.implements[[2]string{f.Repo, owner}]
	if len(types) == 0 || len(types) > maxImplementations {
		return nil
	}
	var out []int32
	for _, t := range types {
		if impl, ok := a.nodeFor(f.Repo, t+"."+method); ok && impl != id {
			out = append(out, impl)
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
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
	for _, m := range a.dispatch().methods[[2]string{f.Repo, f.Name}] {
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
