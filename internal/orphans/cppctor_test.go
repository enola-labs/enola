package orphans

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A C++ constructor and destructor are never called by name. Each runs when an
// object of its class, or of a class derived from it, is created and destroyed, so
// an instantiation of the class is their use.

func cppSym(name, kind, file string, rels ...facts.Relation) facts.Fact {
	f := sym(name, kind)
	f.File = file
	f.Props["language"] = "cpp"
	f.Relations = rels
	return f
}

func TestCppConstructorAndDestructorUsedWhenTheClassIsInstantiated(t *testing.T) {
	got := orphanNames(storeOf(
		cppSym("src/include.db::Holder", facts.SymbolClass, "src/include/holder.hpp"),
		cppSym("src.db::Holder::Holder", facts.SymbolMethod, "src/holder.cpp"),
		cppSym("src.db::Holder::~Holder", facts.SymbolMethod, "src/holder.cpp"),
		cppSym("src.db::Entry", facts.SymbolFunc, "src/entry.cpp",
			facts.Relation{Kind: facts.RelInstantiates, Target: "src/include.db::Holder"}),
	))
	for _, n := range []string{"src.db::Holder::Holder", "src.db::Holder::~Holder"} {
		if got[n] {
			t.Errorf("%s runs whenever Holder is instantiated", n)
		}
	}
}

func TestCppBaseConstructorAndDestructorUsedThroughADerivedInstance(t *testing.T) {
	got := orphanNames(storeOf(
		cppSym("src/include.db::Base", facts.SymbolClass, "src/include/base.hpp"),
		cppSym("src/include.db::Derived", facts.SymbolClass, "src/include/base.hpp",
			facts.Relation{Kind: facts.RelImplements, Target: "src/include.db::Base"}),
		cppSym("src.db::Base::Base", facts.SymbolMethod, "src/base.cpp"),
		cppSym("src.db::Base::~Base", facts.SymbolMethod, "src/base.cpp"),
		cppSym("src.db::Make", facts.SymbolFunc, "src/make.cpp",
			facts.Relation{Kind: facts.RelInstantiates, Target: "src/include.db::Derived"}),
	))
	for _, n := range []string{"src.db::Base::Base", "src.db::Base::~Base"} {
		if got[n] {
			t.Errorf("%s runs for every Derived", n)
		}
	}
}

func TestCppConstructorOfAClassNothingInstantiatesStaysDead(t *testing.T) {
	got := orphanNames(storeOf(
		cppSym("src/include.db::Never", facts.SymbolClass, "src/include/never.hpp"),
		cppSym("src.db::Never::Never", facts.SymbolMethod, "src/never.cpp"),
		cppSym("src.db::Never::~Never", facts.SymbolMethod, "src/never.cpp"),
		cppSym("src.db::Other", facts.SymbolFunc, "src/other.cpp",
			facts.Relation{Kind: facts.RelCalls, Target: "src.db::Never::Run"}),
	))
	for _, n := range []string{"src.db::Never::Never", "src.db::Never::~Never"} {
		if !got[n] {
			t.Errorf("%s: nothing instantiates Never, so it never runs", n)
		}
	}
}
