package cppextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A type name resolves only to a copy the referencing file can include. A lone
// copy out of sight is another program's type that shares the name.

func TestReceiverCallSkipsAnOutOfSightCopyDeclaringTheMember(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"tools/cpp/wrap.h": `namespace cxx { class ScalarFunction { public: void GetSignature(); }; }`,
		"tools/cpp/wrap.cpp": "#include \"wrap.h\"\n" +
			`namespace cxx { void ScalarFunction::GetSignature() {} }`,
		"src/function.hpp": `class BaseFunction { public: void GetSignature(); };
class ScalarFunction : public BaseFunction {};`,
		"src/function.cpp": "#include \"function.hpp\"\n" + `void BaseFunction::GetSignature() {}`,
		"ext/map.cpp": "#include \"../src/function.hpp\"\n" +
			`void Register(ScalarFunction &fn) { fn.GetSignature(); }`,
	})
	reg := mustFact(t, ff, "ext.Register")
	for _, r := range reg.Relations {
		if strings.HasPrefix(r.Target, "tools/cpp.") {
			t.Errorf("ext includes nothing from tools/cpp, got %+v", r)
		}
	}
	if !hasRelation(reg, facts.RelCalls, "src.BaseFunction::GetSignature") {
		t.Errorf("Register should call the inherited BaseFunction::GetSignature, got %+v", reg.Relations)
	}
}

func TestTypeDeclaredOnceOutOfSightIsNotInstantiated(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"parquet/types.hpp": `namespace pq { struct IntType { int v; }; }`,
		"pcg/extras.hpp": `template <typename IntType> IntType fnv(IntType h) {
	return IntType(16777619U) * h;
}`,
	})
	fnv := mustFact(t, ff, "pcg.fnv")
	for _, r := range fnv.Relations {
		if strings.HasPrefix(r.Target, "parquet.") {
			t.Errorf("pcg includes nothing from parquet, got %+v", r)
		}
	}
}

func TestTypeDeclaredOnceInSightStillBinds(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"lib/widget.h": `class Widget { public: Widget(int); void draw(); };`,
		"lib/widget.cpp": "#include \"widget.h\"\n" +
			`Widget::Widget(int) {}
void Widget::draw() {}`,
		"app/main.cpp": "#include \"../lib/widget.h\"\n" +
			`void run() { Widget w = Widget(1); w.draw(); }
class Big : public Widget {};`,
	})
	run := mustFact(t, ff, "app.run")
	if !hasRelation(run, facts.RelInstantiates, "lib.Widget") {
		t.Errorf("run should instantiate lib.Widget, got %+v", run.Relations)
	}
	if !hasRelation(run, facts.RelCalls, "lib.Widget::draw") {
		t.Errorf("run should call lib.Widget::draw, got %+v", run.Relations)
	}
	if big := mustFact(t, ff, "app.Big"); !hasRelation(big, facts.RelImplements, "lib.Widget") {
		t.Errorf("Big should implement lib.Widget, got %+v", big.Relations)
	}
}

// A bare <name.hpp> or an installed <pkg/name.hpp> naming a project header is
// found through a path the extractor does not know, so the file may see every
// directory with a header of that name: a lone copy there still binds.
func TestTypeBehindABareAngleIncludeStillBinds(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"ng/meshing/meshclass.hpp": `namespace netgen { class Mesh { public: void AddPoint(); }; }`,
		"ng/meshing/meshclass.cpp": "#include \"meshclass.hpp\"\n" +
			`namespace netgen { void Mesh::AddPoint() {} }`,
		"ng/nglib.cpp": "#include <meshclass.hpp>\n" +
			`using namespace netgen;
void Ng_AddPoint(Mesh *m) { m->AddPoint(); }`,
		"mobile/draw.cpp": "#include <ngpkg/meshclass.hpp>\n" +
			`void Draw(netgen::Mesh *m) { m->AddPoint(); }`,
	})
	ng := mustFact(t, ff, "ng.Ng_AddPoint")
	if !hasRelation(ng, facts.RelCalls, "ng/meshing.netgen::Mesh::AddPoint") {
		t.Errorf("Ng_AddPoint should call Mesh::AddPoint, got %+v", ng.Relations)
	}
	draw := mustFact(t, ff, "mobile.Draw")
	if !hasRelation(draw, facts.RelCalls, "ng/meshing.netgen::Mesh::AddPoint") {
		t.Errorf("Draw should call Mesh::AddPoint, got %+v", draw.Relations)
	}
}
