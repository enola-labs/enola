package cppextractor

import "testing"

// A method declared `override` (or `final`) in a class whose header sits in one
// directory and defined out of line in another stays an override: the definition
// is the same member, and its file includes the declaring header.

func TestCppOutOfLineDefinitionInAnotherDirectoryKeepsOverride(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/include/catalog/catalog.hpp": `namespace db {
class Catalog { public: virtual void AlterSchema(); virtual void DropSchema(); };
class DuckCatalog : public Catalog { public: void AlterSchema() override; void DropSchema() final; void Own(); };
}`,
		"src/catalog/duck_catalog.cpp": "#include \"catalog/catalog.hpp\"\n" + `namespace db {
void DuckCatalog::AlterSchema() {}
void DuckCatalog::DropSchema() {}
void DuckCatalog::Own() {}
}`,
	})
	alter := mustFact(t, ff, "src/catalog.db::DuckCatalog::AlterSchema")
	if !alter.PropBool("override") {
		t.Errorf("AlterSchema is declared override, got %+v", alter.Props)
	}
	drop := mustFact(t, ff, "src/catalog.db::DuckCatalog::DropSchema")
	if !drop.PropBool("override") || !drop.PropBool("final") {
		t.Errorf("DropSchema is declared final, got %+v", drop.Props)
	}
	own := mustFact(t, ff, "src/catalog.db::DuckCatalog::Own")
	if own.PropBool("override") {
		t.Errorf("Own is no override, got %+v", own.Props)
	}
}

func TestCppOverrideIsNotCarriedFromAHeaderOutOfSight(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"vendor/other/widget.hpp": `namespace db {
class Base { public: virtual void Draw(); };
class Widget : public Base { public: void Draw() override; };
}`,
		"src/ui/widget.hpp": `namespace db { class Widget { public: void Draw(); }; }`,
		"src/ui/widget.cpp": "#include \"widget.hpp\"\n" + `namespace db { void Widget::Draw() {} }`,
	})
	draw := mustFact(t, ff, "src/ui.db::Widget::Draw")
	if draw.PropBool("override") {
		t.Errorf("src/ui includes nothing from vendor/other, got %+v", draw.Props)
	}
}
