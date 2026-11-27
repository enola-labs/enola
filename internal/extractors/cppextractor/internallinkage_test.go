package cppextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A declaration with internal linkage is visible only in its own translation
// unit, so two of one name in two files of one directory are two functions, each
// named for its file (internallinkage.go).

func TestCStaticFunctionsOfOneNameInOneDirectoryStayApart(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"drivers/i2c/adv7180.c": `
static int to_state(int sd) { return sd; }
int adv7180_probe(int sd) { return to_state(sd); }
`,
		"drivers/i2c/tvp5150.c": `
static int to_state(int sd) { return sd + 1; }
int tvp5150_probe(int sd) { return to_state(sd); }
`,
	})
	a := mustFact(t, ff, "drivers/i2c.to_state@adv7180_c")
	b := mustFact(t, ff, "drivers/i2c.to_state@tvp5150_c")
	if a.File != "drivers/i2c/adv7180.c" || b.File != "drivers/i2c/tvp5150.c" {
		t.Errorf("each static keeps its own file, got %s and %s", a.File, b.File)
	}
	if _, ok := findFact(ff, "drivers/i2c.to_state"); ok {
		t.Error("no fact folds the two statics under the shared name")
	}
	if !hasRelation(mustFact(t, ff, "drivers/i2c.adv7180_probe"), facts.RelCalls, "drivers/i2c.to_state@adv7180_c") {
		t.Error("adv7180_probe calls its own file's to_state")
	}
	if !hasRelation(mustFact(t, ff, "drivers/i2c.tvp5150_probe"), facts.RelCalls, "drivers/i2c.to_state@tvp5150_c") {
		t.Error("tvp5150_probe calls its own file's to_state")
	}
}

func TestCStaticPrototypeFusesWithItsDefinition(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.c": `
static int later(int x);
int first(int x) { return later(x); }
int later(int x) { return x; }
`,
	})
	n := 0
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasPrefix(f.Name, "src.later") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the static prototype and its definition are one fact, got %d", n)
	}
	later := mustFact(t, ff, "src.later@a_c")
	if has, _ := later.Props["has_body"].(bool); !has || later.Line != 4 {
		t.Errorf("the definition is authoritative: has_body=%v line=%d", later.Props["has_body"], later.Line)
	}
	if !hasRelation(mustFact(t, ff, "src.first"), facts.RelCalls, "src.later@a_c") {
		t.Error("first calls the file's later")
	}
}

func TestCppAnonymousNamespaceFunctionsStayApart(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.cpp": `
namespace db {
namespace {
int Helper(int x) { return x; }
}
int RunA(int v) { return Helper(v); }
}
`,
		"src/b.cpp": `
namespace db {
namespace {
int Helper(int x) { return x + 1; }
}
int RunB(int v) { return Helper(v); }
}
`,
	})
	if !hasRelation(mustFact(t, ff, "src.db::RunA"), facts.RelCalls, "src.db::Helper@a_cpp") {
		t.Errorf("RunA calls a.cpp's Helper, got %+v", mustFact(t, ff, "src.db::RunA").Relations)
	}
	if !hasRelation(mustFact(t, ff, "src.db::RunB"), facts.RelCalls, "src.db::Helper@b_cpp") {
		t.Errorf("RunB calls b.cpp's Helper, got %+v", mustFact(t, ff, "src.db::RunB").Relations)
	}
}

func TestCStaticInlineHeaderIsQualifiedByTheHeader(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"include/util.h": `
static inline int clamp_u8(int v) { return v > 255 ? 255 : v; }
`,
		"include/user.c": `
#include "util.h"
int near(int v) { return clamp_u8(v); }
`,
		"drv/user.c": `
#include "../include/util.h"
int far(int v) { return clamp_u8(v); }
`,
	})
	mustFact(t, ff, "include.clamp_u8@util_h")
	if !hasRelation(mustFact(t, ff, "include.near"), facts.RelCalls, "include.clamp_u8@util_h") {
		t.Errorf("a caller in the header's directory binds to it, got %+v", mustFact(t, ff, "include.near").Relations)
	}
	if !hasRelation(mustFact(t, ff, "drv.far"), facts.RelCalls, "include.clamp_u8@util_h") {
		t.Errorf("a caller including the header binds to it, got %+v", mustFact(t, ff, "drv.far").Relations)
	}
}

func TestCStaticOfAnotherFileIsNotCallable(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.c": `
static int private_helper(int x) { return x; }
int use_a(int x) { return private_helper(x); }
`,
		"src/b.c": `
int use_b(int x) { return private_helper(x); }
`,
	})
	for _, r := range mustFact(t, ff, "src.use_b").Relations {
		if r.Target == "src.private_helper@a_c" {
			t.Errorf("b.c cannot call a.c's static, got %+v", r)
		}
	}
}

func TestCStaticOfAnIncludedSourceIsCallable(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"lib/impl.c": `
static int inner(int x) { return x; }
`,
		"lib/wrap.c": `
#include "impl.c"
int outer(int x) { return inner(x); }
`,
	})
	if !hasRelation(mustFact(t, ff, "lib.outer"), facts.RelCalls, "lib.inner@impl_c") {
		t.Errorf("a file including the source sees its static, got %+v", mustFact(t, ff, "lib.outer").Relations)
	}
}

func TestCStaticVariablesOfOneNameStayApart(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"fs/a.c": `
static int a_read(void) { return 0; }
static const struct file_operations fops = { .read = a_read };
`,
		"fs/b.c": `
static int b_read(void) { return 0; }
static const struct file_operations fops = { .read = b_read };
`,
	})
	if !hasRelation(mustFact(t, ff, "fs.fops@a_c"), facts.RelCalls, "fs.a_read@a_c") {
		t.Error("a.c's fops references a.c's reader")
	}
	b := mustFact(t, ff, "fs.fops@b_c")
	if hasRelation(b, facts.RelCalls, "fs.a_read@a_c") || !hasRelation(b, facts.RelCalls, "fs.b_read@b_c") {
		t.Errorf("b.c's fops references only b.c's reader, got %+v", b.Relations)
	}
}

func TestCSeveralMainsInOneDirectoryAreSeparatePrograms(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"tools/one.c":  "int main(void) { return 0; }\n",
		"tools/two.c":  "int main(void) { return 1; }\n",
		"single/app.c": "int main(void) { return 0; }\n",
	})
	mustFact(t, ff, "tools.main@one_c")
	mustFact(t, ff, "tools.main@two_c")
	mustFact(t, ff, "single.main") // one program in its directory keeps its name
}
