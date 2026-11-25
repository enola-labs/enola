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
class ByAlias { public: ByAlias(); };
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
namespace base_std = std;
template <class T, class... ARGS>
std::shared_ptr<T> make_alias_ptr(ARGS &&...args) {
	return std::shared_ptr<T>(base_std::make_shared<T>(std::forward<ARGS>(args)...));
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
	auto a = make_alias_ptr<ByAlias>();
	auto c = cast_only<ByDecl>(nullptr);
}
}`)
	run := mustFact(t, ff, "src.db::run")
	for _, want := range []string{"include.db::ByStd", "include.db::ByShared", "include.db::ByUniq", "include.db::ByUniqBase", "include.db::ByWrapped", "include.db::ByAlias"} {
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

// A typedef or alias the caller sees, and a type its own file declares where the
// index has no copy (a struct nested in a class template), name what the source
// means; a class of the same simple name elsewhere is not it.
func TestCppConstructionYieldsToTypedefsAndFileLocalTypes(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"post/shapes.hpp":  `class tetrahedron { public: tetrahedron(); }; class Node { public: Node(); };`,
		"mesh/tet.hpp":     "#include \"../post/shapes.hpp\"\nclass tetmesh { public: typedef double **tetrahedron; void build(); };",
		"mesh/tet.cpp":     "#include \"tet.hpp\"\nvoid tetmesh::build() { tetrahedron t = new double *[4]; tetrahedron *u = new tetrahedron[2]; }",
		"common/rtree.hpp": "#include \"../post/shapes.hpp\"\n#define QUAL RTree<T>\ntemplate <class T> class RTree { struct Node { int n; }; Node *Alloc(); };\ntemplate <class T> typename QUAL::Node *QUAL::Alloc() { return new Node; }\nvoid use() { Node n; }",
		"app/main.cpp":     "#include \"../post/shapes.hpp\"\nvoid run() { Node n; tetrahedron t; }",
	})
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Kind == facts.RelInstantiates && strings.HasPrefix(f.File, "mesh/") || r.Kind == facts.RelInstantiates && strings.HasPrefix(f.File, "common/") {
				t.Errorf("%s does not create a %s", f.Name, r.Target)
			}
		}
	}
	run := mustFact(t, ff, "app.run")
	for _, want := range []string{"post.Node", "post.tetrahedron"} {
		if !hasRelation(run, facts.RelInstantiates, want) {
			t.Errorf("run should instantiate %s, got %+v", want, run.Relations)
		}
	}
}

const memberFactories = `#include <memory>
#include <vector>
namespace db {
class ByArena { public: ByArena(); };
class ByArenaAlias { public: ByArenaAlias(); };
class ByPlanner { public: ByPlanner(int); };
class ByTyped { public: ByTyped(); };
class ByEmplaceLocal { public: ByEmplaceLocal(int); };
class ByEmplaceField { public: ByEmplaceField(int); };
class ByGet { public: ByGet(); };
class Arena {
public:
	template <class T, class... ARGS>
	T *Make(ARGS &&...args) {
		auto mem = Allocate(sizeof(T));
		return new (mem) T(std::forward<ARGS>(args)...);
	}
	template <class T, class... ARGS>
	T *MakeAlias(ARGS &&...args) {
		return Make<T>(std::forward<ARGS>(args)...);
	}
	void *Allocate(int n);
};
class Plan {
public:
	template <class T, class... ARGS>
	T &Make(ARGS &&...args) {
		auto ptr = arena.Make<T>(std::forward<ARGS>(args)...);
		ops.push_back(ptr);
		return *ptr;
	}
	Arena arena;
	std::vector<void *> ops;
};
class Getter {
public:
	template <class T>
	T *Get() { return static_cast<T *>(nullptr); }
};
class Planner {
public:
	template <class T, class... ARGS>
	T &Make(ARGS &&...args) {
		return plan->Make<T>(std::forward<ARGS>(args)...);
	}
	void Build();
	void Typed(Planner &other, Arena &a);
	void Shadow();
	std::unique_ptr<Plan> plan;
	std::vector<ByEmplaceField> items;
};
}
`

func memberFactoryProject(t *testing.T) []facts.Fact {
	t.Helper()
	return extractProject(t, map[string]string{
		"include/arena.hpp": memberFactories,
		"src/planner.cpp": `#include "../include/arena.hpp"
namespace db {
void Planner::Build() {
	Make<ByPlanner>(1);
	items.emplace_back(2);
	std::vector<ByEmplaceLocal> locals;
	locals.emplace_back(3);
	Getter g;
	g.Get<ByGet>();
}
void Planner::Typed(Planner &other, Arena &a) {
	other.Make<ByTyped>();
	a.Make<ByArena>();
	a.MakeAlias<ByArenaAlias>();
}
void Planner::Shadow() {
	auto items = Getter();
	items.emplace_back(4);
}
}
`,
	})
}

// A member function template that news its type parameter, or passes it to such a
// template through the object, a member or a typed receiver, constructs the type
// argument a call writes.
func TestCppMemberFactoryTemplatesInstantiate(t *testing.T) {
	ff := memberFactoryProject(t)
	build := mustFact(t, ff, "src.db::Planner::Build")
	if !hasRelation(build, facts.RelInstantiates, "include.db::ByPlanner") {
		t.Errorf("Build should instantiate ByPlanner through Planner::Make, Plan::Make and Arena::Make, got %+v", build.Relations)
	}
	if hasRelation(build, facts.RelInstantiates, "include.db::ByGet") {
		t.Errorf("a member template that only casts is no factory, got %+v", build.Relations)
	}
	typed := mustFact(t, ff, "src.db::Planner::Typed")
	for _, want := range []string{"include.db::ByTyped", "include.db::ByArena", "include.db::ByArenaAlias"} {
		if !hasRelation(typed, facts.RelInstantiates, want) {
			t.Errorf("Typed should instantiate %s, got %+v", want, typed.Relations)
		}
	}
	for _, f := range ff {
		if f.PropAny(propFieldTemplates) != nil || f.PropAny(propFactoryForwards) != nil {
			t.Errorf("%s keeps a private prop", f.Name)
		}
		for _, r := range f.Relations {
			if r.Kind == relFieldElementCandidate {
				t.Errorf("%s keeps a provisional edge %+v", f.Name, r)
			}
		}
	}
}

// emplace_back and emplace construct the element type of the container a local,
// a parameter or a member of the class declares; a local of the member's name
// hides it.
func TestCppEmplaceInstantiatesElementType(t *testing.T) {
	ff := memberFactoryProject(t)
	build := mustFact(t, ff, "src.db::Planner::Build")
	for _, want := range []string{"include.db::ByEmplaceLocal", "include.db::ByEmplaceField"} {
		if !hasRelation(build, facts.RelInstantiates, want) {
			t.Errorf("Build should instantiate %s, got %+v", want, build.Relations)
		}
	}
	if shadow := mustFact(t, ff, "src.db::Planner::Shadow"); hasRelation(shadow, facts.RelInstantiates, "include.db::ByEmplaceField") {
		t.Errorf("a local hides the member of its name, got %+v", shadow.Relations)
	}
}
