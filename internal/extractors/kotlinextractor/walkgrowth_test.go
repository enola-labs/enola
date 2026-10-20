package kotlinextractor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// growthMembers is enough members that appending their facts outgrows the fact
// slice at least once after the owner's own fact was appended, wherever the
// allocator's capacity boundaries fall.
const growthMembers = 64

func symbolNamed(t *testing.T, ff []facts.Fact, name string) facts.Fact {
	t.Helper()
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Name == name {
			return f
		}
	}
	t.Fatalf("no symbol %s", name)
	return facts.Fact{}
}

// A function whose body declares a local class appends that class's fact while
// the function is still being walked. Every method below does so before it
// instantiates and calls, so each capacity boundary the walk crosses lands
// inside some method's body.
func TestKotlinWalk_FunctionKeepsEdgesWhenItsBodyGrowsTheFacts(t *testing.T) {
	var b strings.Builder
	b.WriteString("package pkg\n\nclass Big {\n    fun helper() {}\n")
	for i := 0; i < growthMembers; i++ {
		fmt.Fprintf(&b, "    fun m%d() {\n        class Local%d\n        Registry()\n        helper()\n    }\n", i, i)
	}
	b.WriteString("}\n")
	ff := extractAST(t, b.String(), false)

	for i := 0; i < growthMembers; i++ {
		m := symbolNamed(t, ff, fmt.Sprintf("pkg.Big.m%d", i))
		if !hasRelation(m, facts.RelInstantiates, "Registry") {
			t.Errorf("m%d lost instantiates Registry: %+v", i, m.Relations)
		}
		if !hasRelation(m, facts.RelCalls, "pkg.Big.helper") {
			t.Errorf("m%d lost calls helper: %+v", i, m.Relations)
		}
	}
}

// Every method appends its fact while the class is walked, so an init block
// after them records its instantiation on a class fact that has moved, and the
// supertype's constructor arguments, walked after the body, do too.
func TestKotlinWalk_ClassKeepsEdgesRecordedAfterItsMembers(t *testing.T) {
	var b strings.Builder
	b.WriteString("package pkg\n\nclass Outer : Base(Seed()) {\n")
	for i := 0; i < growthMembers; i++ {
		fmt.Fprintf(&b, "    fun m%d() {}\n", i)
	}
	b.WriteString("    init {\n        Registry()\n    }\n}\n")
	ff := extractAST(t, b.String(), false)

	outer := symbolNamed(t, ff, "pkg.Outer")
	for _, target := range []string{"Registry", "Seed"} {
		if !hasRelation(outer, facts.RelInstantiates, target) {
			t.Errorf("Outer lost instantiates %s: %+v", target, outer.Relations)
		}
	}
}

// An object declaration is an owner the same way a class is.
func TestKotlinWalk_ObjectKeepsEdgesRecordedAfterItsMembers(t *testing.T) {
	var b strings.Builder
	b.WriteString("package pkg\n\nobject Holder : Base(Seed()) {\n")
	for i := 0; i < growthMembers; i++ {
		fmt.Fprintf(&b, "    fun m%d() {}\n", i)
	}
	b.WriteString("    init {\n        Registry()\n    }\n}\n")
	ff := extractAST(t, b.String(), false)

	holder := symbolNamed(t, ff, "pkg.Holder")
	for _, target := range []string{"Registry", "Seed"} {
		if !hasRelation(holder, facts.RelInstantiates, target) {
			t.Errorf("Holder lost instantiates %s: %+v", target, holder.Relations)
		}
	}
}

// The edges a walk records do not depend on how many facts precede the
// owner: the same class after a varying number of imports carries the same
// relations on every method.
func TestKotlinWalk_EdgesDoNotDependOnWhereTheSliceGrows(t *testing.T) {
	body := "class Big {\n    fun helper() {}\n"
	for i := 0; i < 8; i++ {
		body += fmt.Sprintf("    fun m%d() {\n        class Local%d\n        Registry()\n        helper()\n    }\n", i, i)
	}
	body += "}\n"
	var want map[string][]facts.Relation
	for imports := 0; imports < 20; imports++ {
		var b strings.Builder
		b.WriteString("package pkg\n\n")
		for i := 0; i < imports; i++ {
			fmt.Fprintf(&b, "import org.example.Unused%d\n", i)
		}
		b.WriteString(body)
		got := map[string][]facts.Relation{}
		for _, f := range extractAST(t, b.String(), false) {
			if f.Kind == facts.KindSymbol && strings.HasPrefix(f.Name, "pkg.Big.m") {
				got[f.Name] = f.Relations
			}
		}
		if want == nil {
			want = got
			continue
		}
		for name, rels := range want {
			if fmt.Sprint(got[name]) != fmt.Sprint(rels) {
				t.Errorf("after %d imports %s carries %v, after 0 it carries %v", imports, name, got[name], rels)
			}
		}
	}
}
