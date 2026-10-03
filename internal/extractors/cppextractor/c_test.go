package cppextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// These tests exercise the pure-C path (tree-sitter-c grammar). They reuse the
// helpers in cpp_test.go (extractProject, mustFact, hasRelation, ...).

func TestCFunctionAndCallGraph(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.c": `
int helper(int x) { return x + 1; }
int compute(int x) { return helper(x) + helper(x); }
`,
	})
	h := mustFact(t, ff, "src.helper")
	if sk, _ := h.Props["symbol_kind"].(string); sk != facts.SymbolFunc {
		t.Errorf("helper symbol_kind = %v, want function", sk)
	}
	if h.Props["language"] != langC {
		t.Errorf("helper language = %v, want c", h.Props["language"])
	}
	c := mustFact(t, ff, "src.compute")
	if !hasRelation(c, facts.RelCalls, "src.helper") {
		t.Errorf("compute should call src.helper, got %+v", c.Relations)
	}
}

func TestCStructTypedef(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/t.c": `
struct point { int x; int y; };
typedef struct point point_t;
`,
	})
	p := mustFact(t, ff, "src.point")
	if sk, _ := p.Props["symbol_kind"].(string); sk != facts.SymbolStruct {
		t.Errorf("point should be struct, got %v", sk)
	}
	if p.Props["language"] != langC {
		t.Errorf("point language = %v, want c", p.Props["language"])
	}
	a := mustFact(t, ff, "src.point_t")
	if sk, _ := a.Props["symbol_kind"].(string); sk != facts.SymbolType {
		t.Errorf("point_t should be a type alias, got %v", sk)
	}
}

func TestCEnumNotScoped(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/e.c": `enum color { RED, GREEN, BLUE };`,
	})
	e := mustFact(t, ff, "src.color")
	if sk, _ := e.Props["symbol_kind"].(string); sk != facts.SymbolEnum {
		t.Errorf("color should be enum, got %v", sk)
	}
	if e.Props["scoped"] == true {
		t.Errorf("plain C enum must not be scoped, got %+v", e.Props)
	}
}

func TestCQuotedInclude(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"lib/bar.h": "int bar(void);",
		"lib/use.c": `
#include <stdio.h>
#include "bar.h"
int use(void) { return bar(); }
`,
	})
	var found bool
	for _, f := range ff {
		if f.Kind != facts.KindDependency {
			continue
		}
		inc, _ := f.Props["include"].(string)
		if inc == "stdio.h" {
			t.Errorf("system include <stdio.h> should not yield a dependency fact")
		}
		if inc == "bar.h" {
			found = true
			if f.Props["language"] != langC {
				t.Errorf("bar.h include language = %v, want c", f.Props["language"])
			}
			if f.Props["source"] != "internal" {
				t.Errorf("bar.h should be internal, got %v", f.Props["source"])
			}
			if !hasRelation(f, facts.RelImports, "lib") {
				t.Errorf("bar.h should import module 'lib', got %+v", f.Relations)
			}
		}
	}
	if !found {
		t.Errorf("expected a dependency fact for #include \"bar.h\"")
	}
}

// TestCDesignatedAndRangeInitializers proves the grammar choice matters: C
// designated and GCC range-designated initializers must not abort parsing of
// the enclosing function (tree-sitter-cpp would error on the range form).
func TestCDesignatedAndRangeInitializers(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/init.c": `
struct cfg { int a; int b; };
static const int table[8] = { [0 ... 3] = 1, [4 ... 7] = 2 };
int build(void) {
	struct cfg c = { .a = 1, .b = 2 };
	return c.a + table[0];
}
`,
	})
	// The enclosing function must still be extracted.
	mustFact(t, ff, "src.build")
}

// TestCKeywordIdentifiers is the decisive test: kernel C reuses C++ keywords as
// ordinary identifiers (new, try, class, delete, private, ...). tree-sitter-cpp
// errors on these; tree-sitter-c parses them cleanly. The symbols must appear.
func TestCKeywordIdentifiers(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/kw.c": `
int try;
struct s { int new; int class; int delete; };
void *private(void) { return 0; }
int namespace(int this_) { return this_; }
`,
	})
	mustFact(t, ff, "src.s")
	p := mustFact(t, ff, "src.private")
	if sk, _ := p.Props["symbol_kind"].(string); sk != facts.SymbolFunc {
		t.Errorf("private should be a function, got %v", sk)
	}
	mustFact(t, ff, "src.namespace")
}

// TestCFunctionPointerOpsTable is the kernel case: functions referenced only via
// a struct's function-pointer fields (`.read = foo`) must get an inbound edge
// from the ops-table variable, so they aren't reported as dead code.
func TestCFunctionPointerOpsTable(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/fops.c": `
static int proc_reg_read(void) { return 0; }
static int proc_reg_open(void) { return 0; }
static const struct file_operations proc_reg_file_ops = {
	.read = proc_reg_read,
	.open = proc_reg_open,
};
`,
	})
	ops := mustFact(t, ff, "src.proc_reg_file_ops")
	if sk, _ := ops.Props["symbol_kind"].(string); sk != facts.SymbolConstant {
		t.Errorf("ops table should be a constant (has const), got %v", sk)
	}
	if !hasRelation(ops, facts.RelCalls, "src.proc_reg_read") {
		t.Errorf("ops table should reference proc_reg_read, got %+v", ops.Relations)
	}
	if !hasRelation(ops, facts.RelCalls, "src.proc_reg_open") {
		t.Errorf("ops table should reference proc_reg_open, got %+v", ops.Relations)
	}
}

// TestCFunctionPointerAddressOf covers the `= &func` form for a function-pointer
// variable (whose declarator findFunctionDeclarator would otherwise mistake for a
// prototype).
func TestCFunctionPointerAddressOf(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/cb.c": `
static int handler(void) { return 0; }
static int (*g_cb)(void) = &handler;
`,
	})
	cb := mustFact(t, ff, "src.g_cb")
	if !hasRelation(cb, facts.RelCalls, "src.handler") {
		t.Errorf("g_cb should reference handler via &handler, got %+v", cb.Relations)
	}
}

// TestCFunctionPointerNested covers function refs inside a nested initializer_list.
func TestCFunctionPointerNested(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/cfg.c": `
static int a(void) { return 0; }
static int b(void) { return 0; }
static const struct outer cfg = { .ops = { .x = a, .y = b } };
`,
	})
	cfg := mustFact(t, ff, "src.cfg")
	if !hasRelation(cfg, facts.RelCalls, "src.a") || !hasRelation(cfg, facts.RelCalls, "src.b") {
		t.Errorf("cfg should reference both a and b, got %+v", cfg.Relations)
	}
}

// TestCCallbackArgReference is the second kernel case: a function passed as a call
// argument (callback registration) must get an inbound edge from the caller, so it
// is not reported as dead.
func TestCCallbackArgReference(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/reg.c": `
static int my_show(void) { return 0; }
static int my_init(void) { return register_thing("x", my_show); }
`,
	})
	init := mustFact(t, ff, "src.my_init")
	if !hasRelation(init, facts.RelCalls, "src.my_show") {
		t.Errorf("my_init should reference my_show passed as a callback arg, got %+v", init.Relations)
	}
}

// TestCCallbackArgAddressOf covers `&func` as a call argument.
func TestCCallbackArgAddressOf(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/irq.c": `
static int handler(void) { return 0; }
static int setup(void) { return request_irq(1, &handler, 0); }
`,
	})
	setup := mustFact(t, ff, "src.setup")
	if !hasRelation(setup, facts.RelCalls, "src.handler") {
		t.Errorf("setup should reference handler passed via &handler, got %+v", setup.Relations)
	}
}

// TestCRegistrationMacro covers the dominant kernel false-positive source: a
// function registered only through a file-scope macro invocation (module_init,
// *_initcall, EXPORT_SYMBOL, *_driver). Macros are not expanded, so the
// function-name argument must instead surface as a call edge on the module fact,
// otherwise the entry point is mis-reported as dead.
func TestCRegistrationMacro(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/mem.c": `
static int chr_dev_init(void) { return 0; }
static int read_mem(void) { return 0; }
fs_initcall(chr_dev_init);
EXPORT_SYMBOL(read_mem);
`,
	})
	mod := mustFact(t, ff, "src") // the directory's module fact
	if mod.Kind != facts.KindModule {
		t.Fatalf("expected module fact for dir src, got kind %q", mod.Kind)
	}
	if !hasRelation(mod, facts.RelCalls, "src.chr_dev_init") {
		t.Errorf("module should reference chr_dev_init via fs_initcall, got %+v", mod.Relations)
	}
	if !hasRelation(mod, facts.RelCalls, "src.read_mem") {
		t.Errorf("module should reference read_mem via EXPORT_SYMBOL, got %+v", mod.Relations)
	}
	// The macro name itself must not be emitted as a junk function symbol.
	if _, ok := findFact(ff, "src.fs_initcall"); ok {
		t.Errorf("macro name fs_initcall must not become a function symbol")
	}
	if _, ok := findFact(ff, "src.EXPORT_SYMBOL"); ok {
		t.Errorf("macro name EXPORT_SYMBOL must not become a function symbol")
	}
}

func TestCppFileScopeStaticAssertReferencesConstexprFunction(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/size.cpp": `
constexpr int encodedWords(int n) { return n * 2; }
static_assert(encodedWords(4) == 8);
`,
	})
	mod := mustFact(t, ff, "src")
	if !hasRelation(mod, facts.RelCalls, "src.encodedWords") {
		t.Errorf("module should reference encodedWords from static_assert, got %+v", mod.Relations)
	}
}

func TestCppNonTypeTemplateDefaultReferencesFunction(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/factory.cpp": `
template <typename T>
T * defaultCtor() { return nullptr; }

template <typename T, auto Ctor = defaultCtor<T>>
struct Factory {};
`,
	})
	mod := mustFact(t, ff, "src")
	if !hasRelation(mod, facts.RelCalls, "src.defaultCtor") {
		t.Errorf("module should reference defaultCtor from non-type template default, got %+v", mod.Relations)
	}
}

// TestCRegistrationMacroMultiArg covers DEVICE_ATTR-style macros that mix
// non-function args (a name, a mode literal) with function-name args (show/store):
// only the real functions become edges.
func TestCRegistrationMacroMultiArg(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/attr.c": `
static int show_fn(void) { return 0; }
static int store_fn(void) { return 0; }
DEVICE_ATTR(name, 0644, show_fn, store_fn);
`,
	})
	mod := mustFact(t, ff, "src")
	if !hasRelation(mod, facts.RelCalls, "src.show_fn") || !hasRelation(mod, facts.RelCalls, "src.store_fn") {
		t.Errorf("module should reference show_fn and store_fn, got %+v", mod.Relations)
	}
	if hasRelation(mod, facts.RelCalls, "src.name") {
		t.Errorf("non-function arg `name` must not become a call edge, got %+v", mod.Relations)
	}
}

// TestCFileScopeMacroNoFalseModuleEdges guards the funcNames filter at module
// scope: a macro argument that does not name a real function adds no edge, and a
// file with no registration macros gets no spurious module call edges.
func TestCFileScopeMacroNoFalseModuleEdges(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/p.c": `
static int real(void) { return 0; }
SOME_MACRO(not_a_function);
module_init(real);
`,
	})
	mod := mustFact(t, ff, "src")
	if !hasRelation(mod, facts.RelCalls, "src.real") {
		t.Errorf("module should reference real via module_init, got %+v", mod.Relations)
	}
	if hasRelation(mod, facts.RelCalls, "src.not_a_function") {
		t.Errorf("non-function macro arg must not become a module call edge, got %+v", mod.Relations)
	}
}

// TestCFuncPtrFieldAssignment is the dominant kernel false-positive source:
// callbacks wired into a struct field at runtime inside a probe/init function
// (gpio_chip/irq_chip/pmu_ops). The assigned function must get an inbound edge
// from the enclosing function so it is not reported as dead.
func TestCFuncPtrFieldAssignment(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/gpio.c": `
static int xlp_gpio_set(void) { return 0; }
static int xlp_gpio_get(void) { return 0; }
static int mvebu_mask(void) { return 0; }
static int probe(struct gpio_chip *gc) {
	gc->set = xlp_gpio_set;
	gc->get = &xlp_gpio_get;
	ct->chip.irq_mask = mvebu_mask;
	gc->ngpio = 32;
	return 0;
}
`,
	})
	p := mustFact(t, ff, "src.probe")
	for _, fn := range []string{"src.xlp_gpio_set", "src.xlp_gpio_get", "src.mvebu_mask"} {
		if !hasRelation(p, facts.RelCalls, fn) {
			t.Errorf("probe should reference %s via field assignment, got %+v", fn, p.Relations)
		}
	}
	// A plain data assignment (gc->ngpio = 32) must not invent an edge, and `ngpio`
	// is not a function anyway.
	if hasRelation(p, facts.RelCalls, "src.ngpio") {
		t.Errorf("data assignment must not produce a call edge, got %+v", p.Relations)
	}
}

// TestCStaticRegistrationMacro covers a registration macro that a leading
// `static` qualifier turns into a declaration (parsed as a macro_type_specifier),
// e.g. DEFINE_SIMPLE_DEV_PM_OPS. The function-name args (suspend/resume) must be
// recorded as uses; the ops-table name (not a function) must not.
func TestCStaticRegistrationMacro(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/pm.c": `
static int davinci_suspend(void) { return 0; }
static int davinci_resume(void) { return 0; }
static DEFINE_SIMPLE_DEV_PM_OPS(davinci_pm_ops, davinci_suspend, davinci_resume);
`,
	})
	mod := mustFact(t, ff, "src")
	if !hasRelation(mod, facts.RelCalls, "src.davinci_suspend") || !hasRelation(mod, facts.RelCalls, "src.davinci_resume") {
		t.Errorf("module should reference suspend+resume via DEFINE_SIMPLE_DEV_PM_OPS, got %+v", mod.Relations)
	}
	if hasRelation(mod, facts.RelCalls, "src.davinci_pm_ops") {
		t.Errorf("the ops-table name (not a function) must not become a call edge, got %+v", mod.Relations)
	}
}

// TestCFuncPtrAssignmentNonFunctionDropped: assigning a non-function global to a
// field must not create an edge (funcNames filter), even though it is syntactically
// identical to a callback assignment.
func TestCFuncPtrAssignmentNonFunctionDropped(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.c": `
static int real_cb(void) { return 0; }
static int probe(void) {
	obj->cb = real_cb;
	obj->data = some_global;
	return 0;
}
`,
	})
	p := mustFact(t, ff, "src.probe")
	if !hasRelation(p, facts.RelCalls, "src.real_cb") {
		t.Errorf("probe should reference real_cb, got %+v", p.Relations)
	}
	if hasRelation(p, facts.RelCalls, "src.some_global") {
		t.Errorf("non-function assignment value some_global must not become an edge, got %+v", p.Relations)
	}
}

// TestCCompoundLiteralAssignment covers callbacks wired via an in-body compound
// literal — `cfg = (struct regmap_config){ .lock = fn };` — which is how some
// drivers (e.g. gpio-104-dio-48e regmap config) set up ops at runtime. The
// pointed-to functions must get an edge from the enclosing function.
func TestCCompoundLiteralAssignment(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/r.c": `
static int dio48e_regmap_lock(void) { return 0; }
static int dio48e_regmap_unlock(void) { return 0; }
static int probe(void) {
	cfg = (struct regmap_config) {
		.reg_bits = 8,
		.lock = dio48e_regmap_lock,
		.unlock = dio48e_regmap_unlock,
	};
	return 0;
}
`,
	})
	p := mustFact(t, ff, "src.probe")
	if !hasRelation(p, facts.RelCalls, "src.dio48e_regmap_lock") || !hasRelation(p, facts.RelCalls, "src.dio48e_regmap_unlock") {
		t.Errorf("probe should reference the regmap lock/unlock callbacks, got %+v", p.Relations)
	}
	if hasRelation(p, facts.RelCalls, "src.reg_bits") {
		t.Errorf("a non-function initializer field must not become an edge, got %+v", p.Relations)
	}
}

// TestCMacroBodyCall covers the header-inline case: a function invoked only inside
// a #define replacement list (e.g. an arch size-dispatched cmpxchg helper) is
// invisible to the AST, so we scan the macro body for call-position identifiers.
// The reference lands on the module fact.
func TestCMacroBodyCall(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/cmpxchg.c": `
static int ____cmpxchg_u16(void) { return 0; }
static int ____cmpxchg_u32(void) { return 0; }
static int helper(void) { return 0; }
#define DISPATCH(x) helper(x)
#define CMPX(p, size) (size == 2 ? ____cmpxchg_u16(p) : ____cmpxchg_u32(p))
#define CONST_ONLY 42
`,
	})
	mod := mustFact(t, ff, "src")
	for _, fn := range []string{"src.helper", "src.____cmpxchg_u16", "src.____cmpxchg_u32"} {
		if !hasRelation(mod, facts.RelCalls, fn) {
			t.Errorf("module should reference %s via macro body, got %+v", fn, mod.Relations)
		}
	}
}

// TestCMacroBodyValuePosition covers an ops table defined inside a #define body via
// designated initializers (kernel F7188X_GPIO_BANK style): the function pointers in
// `.field = fn` / `= &fn` value position must be recorded, while comparisons and
// field names must not.
func TestCMacroBodyValuePosition(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/bank.c": `
static int bank_set(void) { return 0; }
static int bank_get(void) { return 0; }
static int bank_irq(void) { return 0; }
#define GPIO_BANK(_n) {            \
		.label = _n,      \
		.set   = bank_set,        \
		.get   = bank_get,        \
		.to_irq = &bank_irq,      \
	}
#define IS_TWO(x) ((x) == bank_set)
`,
	})
	mod := mustFact(t, ff, "src")
	for _, fn := range []string{"src.bank_set", "src.bank_get", "src.bank_irq"} {
		if !hasRelation(mod, facts.RelCalls, fn) {
			t.Errorf("module should reference %s via macro-body designated init, got %+v", fn, mod.Relations)
		}
	}
}

// TestCMacroBodyNoFalseEdges: a macro body with only constants / UPPER_SNAKE names
// must not produce edges.
func TestCMacroBodyNoFalseEdges(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/m.c": `
static int real(void) { return 0; }
#define SIZE 4096
#define FLAGS (GFP_KERNEL | __GFP_ZERO)
#define WRAP(x) real(x)
`,
	})
	mod := mustFact(t, ff, "src")
	if !hasRelation(mod, facts.RelCalls, "src.real") {
		t.Errorf("module should reference real via WRAP macro, got %+v", mod.Relations)
	}
	for _, bad := range []string{"src.SIZE", "src.FLAGS", "src.GFP_KERNEL"} {
		if hasRelation(mod, facts.RelCalls, bad) {
			t.Errorf("constant/macro %s must not become a call edge, got %+v", bad, mod.Relations)
		}
	}
}

// TestCMacroExpansionTokenPaste is the end-to-end token-paste rescue: a configfs-
// style attribute macro defined in one file and invoked in another must have its
// preprocessor-synthesized .show/.store callbacks recorded as used, so they are
// not reported as dead. Exercises the project-wide #define pre-pass + expansion.
func TestCMacroExpansionTokenPaste(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"inc/attr.h": `
#define ATTR_PERM(_pfx, _name, _perm) static struct configfs_attribute _pfx##attr_##_name = { .ca_name = __stringify(_name), .ca_mode = _perm, .show = _pfx##_name##_show, .store = _pfx##_name##_store, }
#define ATTR(_pfx, _name) ATTR_PERM(_pfx, _name, 0644)
`,
		"src/d.c": `
static int cfg_label_show(void) { return 0; }
static int cfg_label_store(void) { return 0; }
ATTR(cfg_, label);
`,
	})
	mod := mustFact(t, ff, "src")
	if !hasRelation(mod, facts.RelCalls, "src.cfg_label_show") || !hasRelation(mod, facts.RelCalls, "src.cfg_label_store") {
		t.Errorf("macro expansion should reference cfg_label_show/store, got %+v", mod.Relations)
	}
}

// TestCMacroExpansionRO covers the single-callback DEVICE_ATTR_RO shape and the
// funcNames guard: the pasted struct-variable name must not become an edge.
func TestCMacroExpansionRO(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/s.c": `
static int temp_show(void) { return 0; }
#define DEV_ATTR_RO(_name) static struct da dev_attr_##_name = { .show = _name##_show }
DEV_ATTR_RO(temp);
`,
	})
	mod := mustFact(t, ff, "src")
	if !hasRelation(mod, facts.RelCalls, "src.temp_show") {
		t.Errorf("DEV_ATTR_RO should reference temp_show, got %+v", mod.Relations)
	}
	if hasRelation(mod, facts.RelCalls, "src.dev_attr_temp") {
		t.Errorf("pasted struct name dev_attr_temp must not be a call edge, got %+v", mod.Relations)
	}
}

// TestCStaticSingleArgAttr covers the `static DEVICE_ATTR_RO(name);` single-arg
// form (parses as a plain declaration, not a macro_type_specifier): the pasted
// _show/_store callbacks must still be recovered via expansion.
func TestCStaticSingleArgAttr(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"inc/sysfs.h": `
#define __ATTR_RO(_name) { .attr = { .name = #_name }, .show = _name##_show }
#define DEVICE_ATTR_RO(_name) static struct device_attribute dev_attr_##_name = __ATTR_RO(_name)
`,
		"src/d.c": `
static int cfam_id_show(void) { return 0; }
static DEVICE_ATTR_RO(cfam_id);
`,
	})
	mod := mustFact(t, ff, "src")
	if !hasRelation(mod, facts.RelCalls, "src.cfam_id_show") {
		t.Errorf("static DEVICE_ATTR_RO(cfam_id) should reference cfam_id_show, got %+v", mod.Relations)
	}
}

// TestCDefineShowAttribute covers a function-defining macro whose referenced
// callback is passed as a CALL ARGUMENT (single_open(file, name_show, ...)); the
// all-identifier scan of the expansion must capture it.
func TestCDefineShowAttribute(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/dbg.c": `
static int component_list_show(void) { return 0; }
#define DEFINE_SHOW_ATTRIBUTE(__name) \
static int __name##_open(void) { return single_open(__name##_show); } \
static const struct file_operations __name##_fops = { .open = __name##_open }
DEFINE_SHOW_ATTRIBUTE(component_list);
`,
	})
	mod := mustFact(t, ff, "src")
	if !hasRelation(mod, facts.RelCalls, "src.component_list_show") {
		t.Errorf("DEFINE_SHOW_ATTRIBUTE should reference component_list_show (call arg), got %+v", mod.Relations)
	}
}

// TestCCapitalizedStaticInlineCall: a real ALL-CAPS/Camel `static inline` function
// called normally in C must be tracked (not mis-routed as a type instantiation and
// dropped). A capitalized value-macro that is not a function must add no edge.
func TestCCapitalizedStaticInlineCall(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/n.c": `
static inline int NE_PTR(int x) { return x; }
static int use(void) { return NE_PTR(3) + ARRAY_SIZE(buf); }
`,
	})
	use := mustFact(t, ff, "src.use")
	if !hasRelation(use, facts.RelCalls, "src.NE_PTR") {
		t.Errorf("use should reference the capitalized static inline NE_PTR, got %+v", use.Relations)
	}
	if hasRelation(use, facts.RelCalls, "src.ARRAY_SIZE") || hasRelation(use, facts.RelInstantiates, "ARRAY_SIZE") {
		t.Errorf("a capitalized value-macro (ARRAY_SIZE) must not become an edge, got %+v", use.Relations)
	}
}

// TestCMachineDescErrorRegion covers the ARM machine_desc pattern: a struct opened
// by a macro (DT_MACHINE_START) and closed by another (MACHINE_END), whose
// `.field = fn` lines tree-sitter renders as a file-scope ERROR node. The callbacks
// must be recovered.
func TestCMachineDescErrorRegion(t *testing.T) {
	// Two blocks after a declaration + a call-valued `.smp` field — the shape that
	// makes tree-sitter recover the blocks as bare assignment_expression /
	// field_expression fragments rather than one clean ERROR node.
	ff := extractProject(t, map[string]string{
		"src/board.c": `
static void omap_reserve(void) {}
static void omap_generic_init(void) {}
static void omap2xxx_restart(void) {}
static void mvebu_dt_init(void) {}
static const char *const compat[] = { "x", 0 };
DT_MACHINE_START(OMAP242X_DT, "Generic OMAP2420")
	.smp		= smp_ops(omap_smp_ops),
	.reserve	= omap_reserve,
	.init_machine	= omap_generic_init,
	.restart	= omap2xxx_restart,
MACHINE_END
DT_MACHINE_START(OMAP243X_DT, "Generic OMAP2430")
	.init_machine	= mvebu_dt_init,
MACHINE_END
`,
	})
	mod := mustFact(t, ff, "src")
	for _, fn := range []string{"src.omap_reserve", "src.omap_generic_init", "src.omap2xxx_restart", "src.mvebu_dt_init"} {
		if !hasRelation(mod, facts.RelCalls, fn) {
			t.Errorf("machine_desc callback %s should be referenced, got %+v", fn, mod.Relations)
		}
	}
}

// TestCArgRefNonFunctionDropped guards the funcNames filter: an argument identifier
// that does not name a real function must NOT produce a RelCalls edge (otherwise a
// data argument could spuriously mark a same-named function as used).
func TestCArgRefNonFunctionDropped(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/u.c": `
static int use(int v) { return v; }
static int caller(void) { int local = 3; return use(local); }
`,
	})
	caller := mustFact(t, ff, "src.caller")
	// The real call to use() is kept; the data argument `local` is not a function.
	if !hasRelation(caller, facts.RelCalls, "src.use") {
		t.Errorf("caller should keep the real call to use, got %+v", caller.Relations)
	}
	if hasRelation(caller, facts.RelCalls, "src.local") {
		t.Errorf("non-function argument `local` must not become a call edge, got %+v", caller.Relations)
	}
}

// TestCArgRefAllCapsSuppressed: an UPPER_SNAKE macro argument must not create an edge.
func TestCArgRefAllCapsSuppressed(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/m.c": `
#define FLAG 1
static int FLAG_fn(void) { return 0; }
static int caller(void) { return take(FLAG); }
`,
	})
	caller := mustFact(t, ff, "src.caller")
	if hasRelation(caller, facts.RelCalls, "src.FLAG") {
		t.Errorf("UPPER_SNAKE argument FLAG must not produce a call edge, got %+v", caller.Relations)
	}
}

// TestCInitializerNonFunctionDropped: a struct field initialized with a lowercase
// NON-function global must not produce a call edge (funcNames filter applies to
// initializer refs too).
func TestCInitializerNonFunctionDropped(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/d.c": `
static int real_fn(void) { return 0; }
static const struct d cfg = { .fn = real_fn, .data = some_global };
`,
	})
	cfg := mustFact(t, ff, "src.cfg")
	if !hasRelation(cfg, facts.RelCalls, "src.real_fn") {
		t.Errorf("cfg should reference real_fn, got %+v", cfg.Relations)
	}
	if hasRelation(cfg, facts.RelCalls, "src.some_global") {
		t.Errorf("non-function initializer value some_global must not become a call edge, got %+v", cfg.Relations)
	}
}

// TestCInitializerNoFalseEdges guards against over-emitting: macro constants
// (UPPER_SNAKE), strings, and numbers in initializers must not become edges.
func TestCInitializerNoFalseEdges(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/c.c": `
#define MAX 8
static const struct cfg c = { .count = MAX, .name = "hi", .n = 3 };
`,
	})
	cf := mustFact(t, ff, "src.c")
	if hasRelation(cf, facts.RelCalls, "src.MAX") {
		t.Errorf("UPPER_SNAKE macro constant MAX must not produce a call edge, got %+v", cf.Relations)
	}
	for _, r := range cf.Relations {
		if r.Kind == facts.RelCalls && (r.Target == "src.hi" || r.Target == "src.n" || r.Target == "src.count" || r.Target == "src.name") {
			t.Errorf("unexpected call edge to %q (designator/literal leaked)", r.Target)
		}
	}
}

// TestCStaticIsFilePrivate verifies C `static` functions are not exported.
func TestCStaticIsFilePrivate(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/s.c": `
static int hidden(void) { return 0; }
int visible(void) { return hidden(); }
`,
	})
	h := mustFact(t, ff, "src.hidden")
	if h.Props["exported"] != false {
		t.Errorf("static C function should have exported=false, got %v", h.Props["exported"])
	}
	if h.Props["static"] != true {
		t.Errorf("hidden should be marked static, got %+v", h.Props)
	}
	v := mustFact(t, ff, "src.visible")
	if v.Props["exported"] != true {
		t.Errorf("non-static C function should be exported=true, got %v", v.Props["exported"])
	}
}

// TestCSameDirHeaderSourceMerge confirms a C prototype (foo.h) and its
// definition (foo.c) in the same directory collapse to a single symbol.
func TestCSameDirHeaderSourceMerge(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"m/foo.h": "int foo(int x);",
		"m/foo.c": "int foo(int x) { return x; }",
	})
	if n := countByName(ff, "m.foo"); n != 1 {
		t.Fatalf("expected a single merged m.foo symbol, got %d", n)
	}
	f := mustFact(t, ff, "m.foo")
	if f.Props["has_body"] != true {
		t.Errorf("merged m.foo should take the definition's has_body=true, got %+v", f.Props)
	}
	if f.Props["language"] != langC {
		t.Errorf("m.foo language = %v, want c", f.Props["language"])
	}
}

// TestHeaderLangAttribution is the regression guard for the Linux-kernel failure
// mode: a stray C++ file in one subtree must NOT flip headers in a sibling C
// subtree to the C++ grammar. The C header here uses `class` as an identifier,
// which only parses under tree-sitter-c.
func TestHeaderLangAttribution(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"tools/x.cpp":   "namespace n { class W {}; }",
		"kernel/y.c":    "int y(void) { return 0; }",
		"include/z.h":   "struct z { int class; int new; };",
		"include/zfn.h": "int zfn(void);",
	})
	// kernel/y.c is C.
	if f := mustFact(t, ff, "kernel.y"); f.Props["language"] != langC {
		t.Errorf("kernel/y.c language = %v, want c", f.Props["language"])
	}
	// include/z.h is routed to C (no C++ sibling in its subtree); the keyword-as-
	// identifier struct proves it: under the C++ grammar this would fail to parse.
	z := mustFact(t, ff, "include.z")
	if z.Props["language"] != langC {
		t.Errorf("include/z.h language = %v, want c (must not be flipped by tools/x.cpp)", z.Props["language"])
	}
	// tools/x.cpp stays C++.
	if f := mustFact(t, ff, "tools.n::W"); f.Props["language"] != langCpp {
		t.Errorf("tools/x.cpp class W language = %v, want cpp", f.Props["language"])
	}
}

// TestCMachineDescCleanErrorRegion covers v15: a single machine_desc block
// (MACHINE_START ... MACHINE_END) alone at file scope, which tree-sitter recovers as
// one clean ERROR node (as opposed to the scattered assignment/field_expression
// fragments of v16). Its `.field = fn` callbacks must still be salvaged onto the
// module fact.
func TestCMachineDescCleanErrorRegion(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/board.c": `
static void example_init(void) {}
static void example_restart(void) {}
MACHINE_START(EXAMPLE, "Example Board")
	.init_machine = example_init,
	.restart      = example_restart,
MACHINE_END
`,
	})
	mod := mustFact(t, ff, "src")
	for _, fn := range []string{"src.example_init", "src.example_restart"} {
		if !hasRelation(mod, facts.RelCalls, fn) {
			t.Errorf("machine_desc clean-ERROR-node callback %s should be salvaged onto the module, got %+v", fn, mod.Relations)
		}
	}
}

// TestCMachineDescSalvageSkipsFunctionBodies covers v17: the full-tree salvage of
// `.field = fn` macro-struct debris must skip function bodies, so an in-body field
// assignment stays attributed to its enclosing function and is NOT double-counted
// onto the module fact.
func TestCMachineDescSalvageSkipsFunctionBodies(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/board.c": `
static void mach_init(void) {}
static int probe_cb(void) { return 0; }
static int probe(struct device *dev) {
	dev->cb = probe_cb;
	return 0;
}
DT_MACHINE_START(EX_DT, "Ex")
	.init_machine = mach_init,
MACHINE_END
`,
	})
	mod := mustFact(t, ff, "src")
	if !hasRelation(mod, facts.RelCalls, "src.mach_init") {
		t.Errorf("module should reference mach_init via machine_desc, got %+v", mod.Relations)
	}
	if hasRelation(mod, facts.RelCalls, "src.probe_cb") {
		t.Errorf("in-body field assignment must NOT be attributed to the module (v17 skips function bodies), got %+v", mod.Relations)
	}
	p := mustFact(t, ff, "src.probe")
	if !hasRelation(p, facts.RelCalls, "src.probe_cb") {
		t.Errorf("probe should own the in-body probe_cb assignment, got %+v", p.Relations)
	}
}

// TestCSyscallDefineBodyCall covers a static helper called only from inside a
// SYSCALL_DEFINE / COMPAT_SYSCALL_DEFINE macro body. tree-sitter parses
// `SYSCALL_DEFINE2(name, ...) { body }` as a detached call_expression PLUS a bare
// top-level compound_statement, so the body's calls have no owning function and
// were dropped entirely — making a static helper reached only from a syscall
// handler look like dead code. The detached-body scan credits the body's
// call-position identifiers to the module owner. -> cache.go v116
func TestCSyscallDefineBodyCall(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/sys.c": `
static int copy_helper(void *dst, const void *src) { return 0; }
static int only_compat_helper(int fd) { return fd; }
static int sigreturn_helper(void) { return 0; }
static int dead_helper(void) { return 0; }

/* Typed params: tree-sitter errors and leaves a detached top-level body. */
SYSCALL_DEFINE2(mysys_settime, struct tv32 __user *, tv,
		struct tz __user *, tz)
{
	int r = copy_helper(tv, tz);
	if (r)
		return -1;
	return 0;
}

COMPAT_SYSCALL_DEFINE1(mysys_compat, int, fd)
{
	return only_compat_helper(fd);
}

/* Zero args: tree-sitter parses a clean function_definition whose declarator is a
 * parenthesized_declarator (no function_declarator), so the body is credited via
 * the handleFunctionDefinition fallback rather than the detached-body case. */
SYSCALL_DEFINE0(mysys_sigreturn)
{
	return sigreturn_helper();
}
`,
	})
	mod := mustFact(t, ff, "src")
	// Every helper reached only from a syscall body must be credited.
	for _, fn := range []string{"src.copy_helper", "src.only_compat_helper", "src.sigreturn_helper"} {
		if !hasRelation(mod, facts.RelCalls, fn) {
			t.Errorf("module should reference %s called from a SYSCALL_DEFINE body, got %+v", fn, mod.Relations)
		}
	}
	// Preservation (GAP §6): a static function called nowhere must NOT be credited,
	// or the fix would hide genuinely dead code.
	if hasRelation(mod, facts.RelCalls, "src.dead_helper") {
		t.Errorf("dead_helper is called nowhere and must stay uncredited")
	}
}
