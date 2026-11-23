package cppextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// Creating an object without the Type(args) expression is an instantiation.

const constructTypes = `namespace db {
class Base { public: Base(); virtual ~Base(); };
class ByDecl { public: ByDecl(int); };
class ByArgs { public: ByArgs(int, int); };
class ByBrace { public: ByBrace(); };
class ByDefault { public: ByDefault(); };
class ByArray { public: ByArray(); };
class ByNew : public Base { public: ByNew(int); };
class ByStd { public: ByStd(); };
class ByShared { public: ByShared(); };
class ByUniq { public: ByUniq(); };
class ByUniqBase : public Base { public: ByUniqBase(); };
class ByWrapped { public: ByWrapped(); };
class ByField { public: ByField(); };
class PtrOnly { public: PtrOnly(); };
class RefOnly { public: RefOnly(); };
class FuncDecl { public: FuncDecl(); };
class Holder { public: ByField field; static PtrOnly shared; PtrOnly *ptr; };
enum Color { Red };
}
`

const constructFactories = `#include <memory>
namespace db {
template <class T, class... ARGS>
std::unique_ptr<T> make_uniq(ARGS &&...args) {
	return std::unique_ptr<T>(new T(std::forward<ARGS>(args)...));
}
template <class S, class T, class... ARGS>
std::unique_ptr<S> make_uniq_base(ARGS &&...args) {
	return std::unique_ptr<S>(new T(std::forward<ARGS>(args)...));
}
template <class T, class... ARGS>
std::unique_ptr<T> make_wrapped(ARGS &&...args) {
	return make_uniq<T>(std::forward<ARGS>(args)...);
}
template <class T, class... ARGS>
std::shared_ptr<T> make_shared_ptr(ARGS &&...args) {
	return std::make_shared<T>(std::forward<ARGS>(args)...);
}
template <class T>
T *cast_only(void *p) {
	return static_cast<T *>(p);
}
}
`

func constructProject(t *testing.T, body string) []facts.Fact {
	t.Helper()
	return extractProject(t, map[string]string{
		"include/types.hpp":   constructTypes,
		"include/helper.hpp":  constructFactories,
		"src/main.cpp":        "#include \"../include/types.hpp\"\n#include \"../include/helper.hpp\"\n" + body,
		"other/elsewhere.hpp": `namespace other { class Hidden { public: Hidden(); }; }`,
	})
}

func TestCppDeclarationInstantiates(t *testing.T) {
	ff := constructProject(t, `namespace db {
void Use(PtrOnly *p);
int run(int x, int y) {
	ByDecl a(1);
	ByArgs b(x, y);
	ByBrace c{};
	ByDefault d;
	ByArray e[3];
	PtrOnly *p = nullptr;
	RefOnly &r = *static_cast<RefOnly *>(nullptr);
	FuncDecl f();
	Color k = Red;
	extern ByStd elsewhere;
	return 0;
}
}`)
	run := mustFact(t, ff, "src.db::run")
	for _, want := range []string{"include.db::ByDecl", "include.db::ByArgs", "include.db::ByBrace", "include.db::ByDefault", "include.db::ByArray"} {
		if !hasRelation(run, facts.RelInstantiates, want) {
			t.Errorf("run should instantiate %s, got %+v", want, run.Relations)
		}
	}
	for _, not := range []string{"include.db::PtrOnly", "include.db::RefOnly", "include.db::FuncDecl", "include.db::Color", "include.db::ByStd"} {
		if hasRelation(run, facts.RelInstantiates, not) {
			t.Errorf("run does not create a %s, got %+v", not, run.Relations)
		}
	}
}

func TestCppNewExpressionInstantiates(t *testing.T) {
	ff := constructProject(t, `namespace db {
Base *run() { return new ByNew(2); }
}`)
	if run := mustFact(t, ff, "src.db::run"); !hasRelation(run, facts.RelInstantiates, "include.db::ByNew") {
		t.Errorf("run should instantiate ByNew, got %+v", run.Relations)
	}
}

func TestCppFactoryTemplatesInstantiate(t *testing.T) {
	ff := constructProject(t, `namespace db {
void run() {
	auto s = std::make_unique<ByStd>();
	auto sh = std::make_shared<ByShared>();
	auto u = make_uniq<ByUniq>();
	auto ub = make_uniq_base<Base, ByUniqBase>();
	auto w = make_wrapped<ByWrapped>();
	auto c = cast_only<ByDecl>(nullptr);
}
}`)
	run := mustFact(t, ff, "src.db::run")
	for _, want := range []string{"include.db::ByStd", "include.db::ByShared", "include.db::ByUniq", "include.db::ByUniqBase", "include.db::ByWrapped"} {
		if !hasRelation(run, facts.RelInstantiates, want) {
			t.Errorf("run should instantiate %s, got %+v", want, run.Relations)
		}
	}
	// make_uniq_base<S, T> constructs T, not S; a template that only casts is no
	// factory.
	for _, not := range []string{"include.db::Base", "include.db::ByDecl"} {
		if hasRelation(run, facts.RelInstantiates, not) {
			t.Errorf("run does not create a %s, got %+v", not, run.Relations)
		}
	}
	// The factories themselves construct only their parameter: no edge, and no
	// provisional edge or prop leaves the extractor.
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Kind == relConstructCandidate || r.Kind == relFactoryCandidate {
				t.Errorf("%s keeps a provisional edge %+v", f.Name, r)
			}
		}
		if f.PropAny(propFactoryNew) != nil || f.PropAny(propFactoryForwards) != nil {
			t.Errorf("%s keeps a factory prop", f.Name)
		}
		if strings.HasPrefix(f.Name, "include.db::make_") {
			for _, r := range f.Relations {
				if r.Kind == facts.RelInstantiates {
					t.Errorf("factory %s instantiates %s", f.Name, r.Target)
				}
			}
		}
	}
}

func TestCppValueMemberInstantiates(t *testing.T) {
	ff := constructProject(t, `namespace db { void run(); }`)
	holder := mustFact(t, ff, "include.db::Holder")
	if !hasRelation(holder, facts.RelInstantiates, "include.db::ByField") {
		t.Errorf("Holder should instantiate its member ByField, got %+v", holder.Relations)
	}
	if hasRelation(holder, facts.RelInstantiates, "include.db::PtrOnly") {
		t.Errorf("a static or pointer member is not constructed with Holder, got %+v", holder.Relations)
	}
}

// A type is bound only through what the source writes and what it can see: a
// template parameter, a type out of sight and a type in another namespace than
// the one written record nothing.
func TestCppConstructionBindsOnlyAWrittenVisibleType(t *testing.T) {
	ff := constructProject(t, `namespace db {
template <class ByDecl> void generic() { ByDecl a; ByDecl *p = new ByDecl(); }
void run() {
	Hidden h;
	other::Hidden h2;
	wrong::ByDefault d;
}
}`)
	generic := mustFact(t, ff, "src.db::generic")
	if hasRelation(generic, facts.RelInstantiates, "include.db::ByDecl") {
		t.Errorf("a template parameter is not the class of that name, got %+v", generic.Relations)
	}
	run := mustFact(t, ff, "src.db::run")
	for _, r := range run.Relations {
		if r.Kind == facts.RelInstantiates {
			t.Errorf("run sees no such type, got %+v", r)
		}
	}
}
