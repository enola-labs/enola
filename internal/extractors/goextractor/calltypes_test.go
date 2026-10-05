package goextractor

import (
	"testing"
)

// shopModule is two packages: resolve declares a type with methods and functions
// returning it, app calls them every way a typed value reaches a method.
func shopModule(app string) map[string]string {
	return map[string]string{
		"go.mod": "module example.com/shop\n\ngo 1.22\n",
		"resolve/resolve.go": `package resolve

type Resolver struct{ Depth int }

func (r Resolver) NodeName(in string) (string, error) { return in, nil }
func (r *Resolver) Reset()                             {}

type Engine struct{ res Resolver }

func (e *Engine) Resolver() Resolver          { return e.res }
func (e *Engine) Pair() (Resolver, error)     { return e.res, nil }
func (e *Engine) Names() []string             { return nil }
func Open(path string) (*Engine, error)      { return &Engine{}, nil }
func Default() Resolver                       { return Resolver{} }

type Source interface {
	Engine() *Engine
}

type Box[T any] struct{ v T }

func (b Box[T]) Get() T { return b.v }
`,
		"app/app.go": "package app\n\nimport \"example.com/shop/resolve\"\n\n" + app,
	}
}

// A parameter's declared type says what a method is called on. This was the
// commonest miss: the call was recorded as the literal "r.NodeName" and reached nothing.
func TestCallOnATypedParameterResolvesToItsMethod(t *testing.T) {
	ff := extractAll(t, shopModule(`
func Analyze(r resolve.Resolver, e *resolve.Engine) (out resolve.Resolver) {
	r.NodeName("x")
	e.Resolver()
	out.Reset()
	func(inner *resolve.Engine) { inner.Names() }(e)
	return out
}
`))
	targets := callTargetsOf(ff, "app.Analyze")
	for _, want := range []string{
		"resolve.Resolver.NodeName", // value parameter
		"resolve.Engine.Resolver",   // pointer parameter
		"resolve.Resolver.Reset",    // named result
		"resolve.Engine.Names",      // a closure's own parameter
	} {
		if !hasTarget(targets, want) {
			t.Errorf("Analyze missing %q; has %v", want, targets)
		}
	}
}

// A call on a call's result is a call on what the first is declared to return.
// These were not recorded at all: the callee was no plain selector chain.
func TestChainedCallResolvesThroughTheDeclaredResult(t *testing.T) {
	ff := extractAll(t, shopModule(`
type server struct{ eng *resolve.Engine }

func (s *server) resolver() resolve.Resolver { return s.eng.Resolver() }

func (s *server) run(src resolve.Source, v any) {
	s.resolver().NodeName("a")
	s.eng.Resolver().NodeName("b")
	resolve.Default().Reset()
	src.Engine().Names()
	v.(*resolve.Engine).Pair()
	(&resolve.Resolver{}).Reset()
}
`))
	targets := callTargetsOf(ff, "app.server.run")
	for _, want := range []string{
		"app.server.resolver",
		"resolve.Resolver.NodeName", // through a method of this package, and through a field's method
		"resolve.Default",
		"resolve.Resolver.Reset", // through a package function
		"resolve.Source.Engine",  // an interface method, by its declaration
		"resolve.Engine.Names",   // and what that is declared to return
		"resolve.Engine.Pair",    // a type assertion states the type
	} {
		if !hasTarget(targets, want) {
			t.Errorf("run missing %q; has %v", want, targets)
		}
	}
}

// A variable assigned from a call has the type the callee declares, one result or several.
func TestLocalAssignedFromACallTakesItsDeclaredResult(t *testing.T) {
	ff := extractAll(t, shopModule(`
func Load(path string) {
	eng, err := resolve.Open(path)
	if err != nil {
		return
	}
	r := eng.Resolver()
	r.NodeName("x")
	other, _ := eng.Pair()
	other.Reset()
}
`))
	targets := callTargetsOf(ff, "app.Load")
	for _, want := range []string{"resolve.Open", "resolve.Engine.Resolver", "resolve.Resolver.NodeName", "resolve.Engine.Pair", "resolve.Resolver.Reset"} {
		if !hasTarget(targets, want) {
			t.Errorf("Load missing %q; has %v", want, targets)
		}
	}
}

// The table of local types knows nothing of scope. A name that is a parameter and is
// then declared again, as something the source does not give a type for, is used for
// two things, and a call on it must not be sent to the parameter's type.
func TestRedeclaredParameterIsNotResolved(t *testing.T) {
	ff := extractAll(t, shopModule(`
type row struct{}

func (row) Scan() {}

func Each(r resolve.Resolver, rows []row, e *resolve.Engine) {
	for _, r := range rows {
		r.Scan()
	}
	func(e row) { e.Scan() }(rows[0])
}
`))
	targets := callTargetsOf(ff, "app.Each")
	for _, wrong := range []string{"resolve.Resolver.Scan", "resolve.Engine.Scan"} {
		if hasTarget(targets, wrong) {
			t.Errorf("Each has %q, a method the shadowed parameter's type does not have; targets %v", wrong, targets)
		}
	}
}

// Nothing is claimed where the source states no named type: a predeclared type, a
// type parameter, a variadic, a result that is a slice, a function this module does
// not declare.
func TestNoTypeIsInventedWhereNoneIsDeclared(t *testing.T) {
	ff := extractAll(t, shopModule(`
import "strings"

func Loose[T any](err error, item T, many ...resolve.Resolver) {
	err.Error()
	item.String()
	many.Len()
	b := resolve.Box[int]{}
	b.Get().Bit()
	e, _ := resolve.Open("p")
	e.Names().Sort()
	strings.NewReplacer().Replace("x")
}
`))
	targets := callTargetsOf(ff, "app.Loose")
	for _, wrong := range []string{
		"app.error.Error", "app.T.String", "resolve.Resolver.Len", // predeclared, type parameter, variadic
		"resolve.T.Bit", "app.T.Bit", "app.int.Bit", // a generic method's result is its type parameter
		"resolve.string.Sort", "app.string.Sort", // a slice result names no type
	} {
		if hasTarget(targets, wrong) {
			t.Errorf("Loose has invented target %q; targets %v", wrong, targets)
		}
	}
	// What was recorded before still is.
	for _, want := range []string{"err.Error", "resolve.Open", "resolve.Engine.Names", "strings.NewReplacer"} {
		if !hasTarget(targets, want) {
			t.Errorf("Loose missing %q; has %v", want, targets)
		}
	}
}

// An alias is another name for a type. A value declared under it has that type's
// methods, and they are declared under the type, so that is where a call lands.
func TestCallThroughATypeAliasLandsOnTheAliasedType(t *testing.T) {
	files := shopModule(`
import "example.com/shop/public"

type local = resolve.Resolver

func Use(e public.Engine, r public.Resolver, l local, n public.Names, sig *public.Signature) {
	sig.Decode()
	e.Resolver().NodeName("a") // an aliased type, then what its method returns
	r.Reset()                  // an alias of an alias
	l.NodeName("b")            // an alias declared in this package
	public.Open("p")
	n.Len()
}
`)
	files["public/public.go"] = `package public

import "example.com/shop/resolve"

type Engine = resolve.Engine
type Mid = resolve.Resolver
type Resolver = Mid

// A defined type is a new type: it has none of the methods of what it is built on.
type Names []string

func (Names) Len() int { return 0 }

func Open(p string) (*Engine, error) { return resolve.Open(p) }
`
	// One name declared both ways, as build-tagged files do. Both are read, and the
	// methods are declared under the package's own name.
	files["public/sig_a.go"] = "//go:build a\n\npackage public\n\nimport \"example.com/shop/resolve\"\n\ntype Signature = resolve.Resolver\n"
	files["public/sig_b.go"] = "//go:build !a\n\npackage public\n\ntype Signature struct{}\n\nfunc (s *Signature) Decode() {}\n"
	ff := extractAll(t, files)
	targets := callTargetsOf(ff, "app.Use")
	for _, want := range []string{
		"resolve.Engine.Resolver", "resolve.Resolver.NodeName", "resolve.Resolver.Reset",
		"public.Open",             // a function is where it is declared; only types are aliased
		"public.Names.Len",        // and a defined type keeps its own name
		"public.Signature.Decode", // as does a name that is an alias in one file and a type in another
	} {
		if !hasTarget(targets, want) {
			t.Errorf("Use missing %q; has %v", want, targets)
		}
	}
	for _, wrong := range []string{"public.Engine.Resolver", "public.Resolver.Reset", "public.Mid.Reset", "app.local.NodeName"} {
		if hasTarget(targets, wrong) {
			t.Errorf("Use has %q, a method named under an alias; targets %v", wrong, targets)
		}
	}
}
