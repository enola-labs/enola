package cppextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A bare name in C never names a struct member: `cmd(ctrl, 1)` calls a parameter
// or local of that name, and a function-pointer member is called through its
// object (`ctrl->cmd(…)`).
func TestCBareNameDoesNotBindAStructMember(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"drv/ops.h": "struct ctl { int (*cmd)(struct ctl *c, int op); };\n",
		"drv/core.c": "#include \"ops.h\"\n" +
			"static int do_cmd(struct ctl *c, int (*cmd)(struct ctl *, int)) { return cmd(c, 1); }\n" +
			"static int helper(int x) { return x; }\n" +
			"int entry(struct ctl *c) { return do_cmd(c, 0) + helper(2); }\n",
	})
	if !hasRelation(mustFact(t, ff, "drv.entry"), facts.RelCalls, "drv.helper") {
		t.Errorf("a bare call to a function still binds")
	}
	for _, r := range mustFact(t, ff, "drv.do_cmd").Relations {
		if strings.Contains(r.Target, "::") {
			t.Errorf("do_cmd calls its parameter, not %s", r.Target)
		}
	}
}

// In C++ a bare name reaches a member only from inside a member function (its own
// class or a base); a free function's bare call is never a member.
func TestCppFreeFunctionBareNameDoesNotBindAMember(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.cpp": `struct Base { void reset(); };
void Base::reset() {}
struct Derived : Base { void run(); };
void Derived::run() { reset(); }
void freefn(void (*reset)()) { reset(); }
`,
	})
	if !hasRelation(mustFact(t, ff, "src.Derived::run"), facts.RelCalls, "src.Base::reset") {
		t.Errorf("a member function's bare call still reaches an inherited member, got %+v", mustFact(t, ff, "src.Derived::run").Relations)
	}
	for _, r := range mustFact(t, ff, "src.freefn").Relations {
		if r.Target == "src.Base::reset" {
			t.Errorf("freefn calls its parameter, not Base::reset")
		}
	}
}
