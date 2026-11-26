package cppextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

const chainFactories = `#include <memory>
namespace db {
class ByForward { public: ByForward(); };
class ByParamChain { public: ByParamChain(); };
class ByAlias { public: ByAlias(); };
class ByPtrChain { public: ByPtrChain(); };
class ByBaseField { public: ByBaseField(); };
class ByBaseChain { public: ByBaseChain(); };
class ByInherited { public: ByInherited(); };
class ByNoField { public: ByNoField(); };
class ByScalar { public: ByScalar(); };
class ByShadow { public: ByShadow(); };
class ByHidden { public: ByHidden(); };
class Arena {
public:
	template <class T, class... ARGS>
	T *Make(ARGS &&...args) {
		return new T(std::forward<ARGS>(args)...);
	}
};
class SubArena : public Arena {};
class OwnMake : public Arena {
public:
	template <class T>
	T *Make();
};
struct Context {
	Arena &process;
	Arena *other;
	SubArena sub;
	OwnMake own;
	int count;
};
class State {
public:
	template <class P, class... ARGS>
	P *Make(ARGS &&...args);
	Context &context;
};
template <class P, class... ARGS>
P *State::Make(ARGS &&...args) {
	return context.process.Make<P>(std::forward<ARGS>(args)...);
}
struct Input {
	State &state;
};
struct Frame {
	State &state;
};
class Derived : public Frame {
public:
	void Push();
	void Shadow();
};
class Runner {
public:
	void Run(State &state, Input input);
};
}
`

func chainFactoryProject(t *testing.T) []facts.Fact {
	t.Helper()
	return extractProject(t, map[string]string{
		"include/chain.hpp": chainFactories,
		"src/run.cpp": `#include "../include/chain.hpp"
namespace db {
void Runner::Run(State &state, Input input) {
	state.Make<ByForward>();
	input.state.context.process.Make<ByParamChain>();
	auto &arena = input.state.context.process;
	arena.Make<ByAlias>();
	input.state.context.other->Make<ByPtrChain>();
	input.state.context.sub.Make<ByInherited>();
	input.state.missing.Make<ByNoField>();
	input.state.context.own.Make<ByHidden>();
	input.state.context.count.Make<ByScalar>();
}
void Derived::Push() {
	state.Make<ByBaseField>();
	state.context.process.Make<ByBaseChain>();
}
void Derived::Shadow() {
	auto state = Input();
	state.Make<ByShadow>();
}
}
`,
	})
}

// A member template reached through a chain of members (a.b.c.Make<T>()), through
// an auto local bound to such a chain, or through a member the caller's class
// inherits, constructs its type argument; a member template a class inherits is
// found on its base.
func TestCppChainedReceiverFactoriesInstantiate(t *testing.T) {
	ff := chainFactoryProject(t)
	run := mustFact(t, ff, "src.db::Runner::Run")
	for _, want := range []string{"ByForward", "ByParamChain", "ByAlias", "ByPtrChain", "ByInherited"} {
		if !hasRelation(run, facts.RelInstantiates, "include.db::"+want) {
			t.Errorf("Run should instantiate %s, got %+v", want, run.Relations)
		}
	}
	for _, not := range []string{"ByNoField", "ByScalar", "ByHidden"} {
		if hasRelation(run, facts.RelInstantiates, "include.db::"+not) {
			t.Errorf("a chain through no class member, or to a class whose own Make hides the base factory, reaches none: %s, got %+v", not, run.Relations)
		}
	}
	push := mustFact(t, ff, "src.db::Derived::Push")
	for _, want := range []string{"ByBaseField", "ByBaseChain"} {
		if !hasRelation(push, facts.RelInstantiates, "include.db::"+want) {
			t.Errorf("Push should instantiate %s through a member of its base, got %+v", want, push.Relations)
		}
	}
	if shadow := mustFact(t, ff, "src.db::Derived::Shadow"); hasRelation(shadow, facts.RelInstantiates, "include.db::ByShadow") {
		t.Errorf("a local hides the inherited member of its name, got %+v", shadow.Relations)
	}
}

const chainEmplace = `#include <vector>
#include <memory>
namespace db {
class ByMemberChain { public: ByMemberChain(); };
class ByThisChain { public: ByThisChain(int); };
class ByParamChain { public: ByParamChain(); };
class ByBaseChain { public: ByBaseChain(); };
class ByAliasChain { public: ByAliasChain(); };
class ByShadow { public: ByShadow(); };
struct Inner {
	std::vector<ByMemberChain> items;
	std::vector<ByThisChain> bars;
	std::vector<ByParamChain> params;
	std::vector<ByBaseChain> based;
	std::vector<ByAliasChain> aliased;
	std::vector<ByShadow> shadowed;
	int count;
};
struct Holder { std::unique_ptr<Inner> inner; };
struct Base { Inner base_in; };
struct Outer : public Base {
	Inner in;
	Holder holder;
	void Member();
	void Param(Holder &h);
	void Shadow();
};
}
`

func chainEmplaceProject(t *testing.T) []facts.Fact {
	t.Helper()
	return extractProject(t, map[string]string{
		"include/chain.hpp": chainEmplace,
		"src/emplace.cpp": `#include "../include/chain.hpp"
namespace db {
void Outer::Member() {
	in.items.emplace_back();
	this->holder.inner->bars.emplace_back(1);
	base_in.based.emplace_back();
	auto &alias = holder.inner;
	alias->aliased.emplace_back();
	in.count.emplace_back();
}
void Outer::Param(Holder &h) {
	h.inner->params.emplace_back();
}
void Outer::Shadow() {
	auto in = Holder();
	in.shadowed.emplace_back();
}
}
`,
	})
}

// An emplace on a container reached through a chain of members (a.b.items, this->a.b,
// a typed parameter's member, an inherited member, an auto alias of a chain)
// constructs the container's element type, as one on a member of the caller's
// class does.
func TestCppEmplaceThroughMemberChainInstantiates(t *testing.T) {
	ff := chainEmplaceProject(t)
	member := mustFact(t, ff, "src.db::Outer::Member")
	for _, want := range []string{"ByMemberChain", "ByThisChain", "ByBaseChain", "ByAliasChain"} {
		if !hasRelation(member, facts.RelInstantiates, "include.db::"+want) {
			t.Errorf("Member should instantiate %s, got %+v", want, member.Relations)
		}
	}
	if param := mustFact(t, ff, "src.db::Outer::Param"); !hasRelation(param, facts.RelInstantiates, "include.db::ByParamChain") {
		t.Errorf("Param should instantiate ByParamChain, got %+v", param.Relations)
	}
	if shadow := mustFact(t, ff, "src.db::Outer::Shadow"); hasRelation(shadow, facts.RelInstantiates, "include.db::ByShadow") {
		t.Errorf("a local hides the member of its name, got %+v", shadow.Relations)
	}
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Kind == relFieldElementCandidate {
				t.Errorf("%s keeps a provisional edge %+v", f.Name, r)
			}
		}
	}
}
