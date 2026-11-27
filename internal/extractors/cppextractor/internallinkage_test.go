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

// A source another source includes at its end (a unit test, a split-out half)
// shares the including file's translation unit, so it sees that file's statics; a
// header's inline code does not, whatever source includes it.
func TestCIncludedSourceSeesTheIncludingFilesStatics(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"lib/core.c": `
static int parse(int x) { return x; }
#include "core_test.c"
`,
		"lib/core_test.c": `
int test_parse(void) { return parse(1); }
`,
		"lib/widget.h": `
static inline int widget_draw(int x) { return draw(x); }
`,
		"lib/draw.c": `
#include "widget.h"
static int draw(int x) { return x; }
`,
	})
	if !hasRelation(mustFact(t, ff, "lib.test_parse"), facts.RelCalls, "lib.parse@core_c") {
		t.Errorf("the included test calls the including file's static, got %+v", mustFact(t, ff, "lib.test_parse").Relations)
	}
	if hasRelation(mustFact(t, ff, "lib.widget_draw@widget_h"), facts.RelCalls, "lib.draw@draw_c") {
		t.Error("a header's inline code does not bind to a static of a source including it")
	}
}

// A header's static is found by its name from another directory only when no
// source file declares a static of that name too: a libc shim's `lseek` is not
// what a program calling the C library's lseek means. A source included from
// another directory shares its statics with the including file alone.
func TestCHeaderStaticWithASourceNamesakeIsNotFoundByName(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"tools/include/nolibc/sys.h": `
static int lseek(int fd, int off, int whence) { return off; }
`,
		"tools/tracing/own.c": `
static int lseek(int fd, int off, int whence) { return 0; }
int seek_own(int fd) { return lseek(fd, 0, 0); }
`,
		"arch/boot/tools/mkbb.c": `
int main(void) { return lseek(3, 0, 0); }
`,
		"lib/decompress.c": `
static int copy_bytes(int n) { return n; }
`,
		"arch/boot/unpack.c": `
#include "../../lib/decompress.c"
int unpack(int n) { return copy_bytes(n); }
`,
		"drivers/accel/ms.c": `
int leftover(int n) { return copy_bytes(n); }
`,
	})
	for _, r := range mustFact(t, ff, "arch/boot/tools.main").Relations {
		if r.Target == "tools/include/nolibc.lseek@sys_h" {
			t.Errorf("a program not including the shim does not call it, got %+v", r)
		}
	}
	if !hasRelation(mustFact(t, ff, "arch/boot.unpack"), facts.RelCalls, "lib.copy_bytes@decompress_c") {
		t.Errorf("the including file calls the included source's static, got %+v", mustFact(t, ff, "arch/boot.unpack").Relations)
	}
	if hasRelation(mustFact(t, ff, "drivers/accel.leftover"), facts.RelCalls, "lib.copy_bytes@decompress_c") {
		t.Error("a file not including the source does not see its static")
	}
}

// Two directories each include their own copy of a source by one file name; each
// copy's code sees the statics of the file that includes it.
func TestCIncludedSourceCopiesSeeTheirOwnIncluder(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"mesh/recover.cpp": "static int orient(int x) { return x; }\n#include \"tetgen.cxx\"\n",
		"mesh/tetgen.cxx":  "int tet_mesh(int x) { return orient(x); }\n",
		"hxt/recover2.cpp": "static int orient(int x) { return x; }\n#include \"tetgen.cxx\"\n",
		"hxt/tetgen.cxx":   "int tet_hxt(int x) { return orient(x); }\n",
	})
	if !hasRelation(mustFact(t, ff, "mesh.tet_mesh"), facts.RelCalls, "mesh.orient@recover_cpp") {
		t.Errorf("mesh's copy calls mesh's static, got %+v", mustFact(t, ff, "mesh.tet_mesh").Relations)
	}
	if !hasRelation(mustFact(t, ff, "hxt.tet_hxt"), facts.RelCalls, "hxt.orient@recover2_cpp") {
		t.Errorf("hxt's copy calls hxt's static, got %+v", mustFact(t, ff, "hxt.tet_hxt").Relations)
	}
}

// A header's prototype of a static the source including it defines is that
// definition, as an extern prototype is its definition.
func TestCHeaderPrototypeOfASourceStaticFusesWithIt(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"post/x3d.h":   "static void write_center(int fp);\n",
		"post/x3d.cpp": "#include \"x3d.h\"\nstatic void write_center(int fp) {}\nvoid write_all(int fp) { write_center(fp); }\n",
	})
	n := 0
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasPrefix(f.Name, "post.write_center") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the header prototype and the source definition are one fact, got %d", n)
	}
	def := mustFact(t, ff, "post.write_center@x3d_cpp")
	if def.File != "post/x3d.cpp" {
		t.Errorf("the definition is authoritative, got %s", def.File)
	}
}

// A driver that includes its core source and two halves of one unit: the
// prototype one half declares, and the header's table references, are the
// definition the other half holds.
func TestCStaticPrototypeInOneUnitFusesWithItsDefinition(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"usb/hcd.c":   "#include \"hub.c\"\n#include \"queue.c\"\n",
		"usb/hub.c":   "static void unlink_async(int h);\nvoid hub_suspend(int h) { unlink_async(h); }\n",
		"usb/queue.c": "static void unlink_async(int h) {}\n",
		"cam/st.h":    "static int st_start(int sd);\nstatic const struct ops st_ops = { .start = st_start };\n",
		"cam/st.c":    "#include \"st.h\"\nstatic int st_start(int sd) { return sd; }\n",
	})
	if !hasRelation(mustFact(t, ff, "usb.hub_suspend"), facts.RelCalls, "usb.unlink_async@queue_c") {
		t.Errorf("the prototype's caller calls the definition, got %+v", mustFact(t, ff, "usb.hub_suspend").Relations)
	}
	if _, ok := findFact(ff, "usb.unlink_async@hub_c"); ok {
		t.Error("the prototype is no fact of its own")
	}
	if !hasRelation(mustFact(t, ff, "cam.st_ops@st_h"), facts.RelCalls, "cam.st_start@st_c") {
		t.Errorf("the header's table references the definition, got %+v", mustFact(t, ff, "cam.st_ops@st_h").Relations)
	}
}

// A static's prototype in the file that defines it stays that definition, though
// another directory's file of the same name defines a static of that name.
func TestCStaticPrototypeStaysWithItsOwnFile(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"kernel/signal.c":  "static void *sig_handler(int sig);\nint do_sigaction(int sig) { return sig_handler(sig) != 0; }\nstatic void *sig_handler(int sig) { return 0; }\n",
		"arch/um/signal.c": "static void sig_handler(int sig) {}\n",
		"arch/um/main.c":   "#include \"signal.c\"\n",
	})
	if !hasRelation(mustFact(t, ff, "kernel.do_sigaction"), facts.RelCalls, "kernel.sig_handler@signal_c") {
		t.Errorf("do_sigaction calls its own file's sig_handler, got %+v", mustFact(t, ff, "kernel.do_sigaction").Relations)
	}
}

// Two headers of one directory define a static of one name, alternatives a
// configuration picks between: a file calls the one it includes itself. A header's
// prototype of a static another header of its directory defines, and includes, is
// that definition.
func TestCHeaderStaticAlternativesBindByTheCallersInclude(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"asm/msr.h":             "static inline void wrmsrq(int m) {}\n",
		"asm/paravirt.h":        "static inline void wrmsrq(int m) {}\n",
		"lib/msr_user.c":        "#include <asm/msr.h>\nvoid set_msr(int m) { wrmsrq(m); }\n",
		"mm/highmem.h":          "#include \"highmem-internal.h\"\nstatic inline void *kmap_local_page(int page);\n",
		"mm/highmem-internal.h": "static inline void *kmap_local_page(int page) { return 0; }\n",
		"fs/read.c":             "#include \"../mm/highmem.h\"\nvoid *map(int p) { return kmap_local_page(p); }\n",
	})
	if !hasRelation(mustFact(t, ff, "lib.set_msr"), facts.RelCalls, "asm.wrmsrq@msr_h") {
		t.Errorf("set_msr calls the wrmsrq of the header it includes, got %+v", mustFact(t, ff, "lib.set_msr").Relations)
	}
	if _, ok := findFact(ff, "mm.kmap_local_page@highmem_h"); ok {
		t.Error("the prototype is no fact of its own")
	}
	if !hasRelation(mustFact(t, ff, "fs.map"), facts.RelCalls, "mm.kmap_local_page@highmem-internal_h") {
		t.Errorf("map calls the definition behind the prototype, got %+v", mustFact(t, ff, "fs.map").Relations)
	}
}
