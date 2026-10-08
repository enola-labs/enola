package cppextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// tree-sitter recovers from source it cannot parse by attaching what follows to
// whatever is still open. These tests pin that the definitions it did produce are
// extracted from where recovery left them.

// An unclosed block makes every later definition a child of the function's body.
func TestDefinitionsSwallowedByAnUnclosedFunctionAreExtracted(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/x.cpp": `void helper();
void first(int x)
{
  if (x) {
    x++;
}

int second(int n) {
  if (n > 1) { helper(); }
  return n;
}
int third() { return second(2); }
`,
	})
	second := mustFact(t, ff, "src.second")
	if !hasRelation(second, facts.RelCalls, "src.helper") {
		t.Errorf("src.second should own its call to helper, got %+v", second.Relations)
	}
	third := mustFact(t, ff, "src.third")
	if !hasRelation(third, facts.RelCalls, "src.second") {
		t.Errorf("src.third should call src.second, got %+v", third.Relations)
	}
	first := mustFact(t, ff, "src.first")
	for _, stolen := range []string{"src.helper", "src.second"} {
		if hasRelation(first, facts.RelCalls, stolen) {
			t.Errorf("src.first was credited with a call to %s made by a later function: %+v", stolen, first.Relations)
		}
	}
	if got := first.PropAny("cyclomatic"); got != 2 {
		t.Errorf("src.first cyclomatic = %v, want 2: only its own if counts", got)
	}
}

// A brace opened in both branches of a preprocessor guard cannot be balanced, and
// the definitions after it land under an ERROR node.
func TestDefinitionsUnderAnErrorNodeAreExtracted(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/x.cpp": `int first(int a)
{
#ifdef FAST
  if (a) {
#else
  if (!a) {
#endif
    a++;
  }
  return a;
}

class Shape { public: int area(); };
int second() { return 1; }
int third() { return second(); }
`,
	})
	for _, name := range []string{"src.second", "src.third", "src.Shape"} {
		mustFact(t, ff, name)
	}
	if third := mustFact(t, ff, "src.third"); !hasRelation(third, facts.RelCalls, "src.second") {
		t.Errorf("src.third should call src.second, got %+v", third.Relations)
	}
}

// A method defined inline in a local class is part of the function that declares
// the class. It is not a swallowed definition.
func TestLocalClassMethodsStayWithTheirFunction(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/x.cpp": `void helper();
void outer() {
  struct Local {
    void run() { helper(); }
  };
  Local local;
  local.run();
}
`,
	})
	if _, found := findFact(ff, "src.run"); found {
		t.Errorf("a local class method was emitted as a free function src.run")
	}
	if outer := mustFact(t, ff, "src.outer"); !hasRelation(outer, facts.RelCalls, "src.helper") {
		t.Errorf("src.outer should keep the call made inside its local class, got %+v", outer.Relations)
	}
}

// Prose inside a disabled block can make recovery read a following statement as a
// definition. A "function" named after a statement keyword is not one.
func TestMisreadStatementIsNotExtractedAsADefinition(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/x.c": `void helper(void);
void outer(int rval)
{
    if (rval) {
        helper();
    }
#if 0
  - this is just to check that the code is finding all cuts
            if (rval) {
                helper();
            }
#endif
}
`,
	})
	if _, found := findFact(ff, "src.if"); found {
		t.Errorf("a statement was extracted as a function named if")
	}
	mustFact(t, ff, "src.outer")
}

// Two loop macros in a row, `for_each_net(net) for_each_netdev(net, dev) { ... }`,
// parse as a function whose return type is the first macro. That is ordinary body
// code in a function that parsed cleanly, and must stay so.
func TestStackedLoopMacrosStayPartOfTheirFunction(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/x.c": `struct net;
struct net_device;
void use(struct net_device *dev);
void walk(struct net *net, struct net_device *dev)
{
	for_each_net(net)
		for_each_netdev(net, dev) {
			use(dev);
		}
}
`,
	})
	if _, found := findFact(ff, "src.for_each_netdev"); found {
		t.Errorf("a loop macro was extracted as a function")
	}
	if walk := mustFact(t, ff, "src.walk"); !hasRelation(walk, facts.RelCalls, "src.use") {
		t.Errorf("src.walk should keep the call inside its macro loop, got %+v", walk.Relations)
	}
}

// A loop macro with a block, `for_each_possible_cpu(i) { ... }`, parses as a
// definition with no function declarator. Inside a body it is the body's own code.
func TestLoopMacroBlockStaysPartOfItsFunction(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/x.c": `void register_cpu(int cpu);
int register_cpus(void)
{
	int i;

	for_each_possible_cpu(i) {
		register_cpu(i);
	}
	return 0;
}
`,
	})
	if cpus := mustFact(t, ff, "src.register_cpus"); !hasRelation(cpus, facts.RelCalls, "src.register_cpu") {
		t.Errorf("src.register_cpus should keep the call inside its macro loop, got %+v", cpus.Relations)
	}
}

// An attribute macro on a local, `be128 __aligned(8) lengths;`, reads as a K&R
// definition of a function named __aligned whose "parameter" is a literal, with
// the statements up to the next block as its body.
func TestAttributeMacroOnALocalIsNotADefinition(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/x.c": `void fill(void *p, int n);
void finish(void *p);
static int crypt(struct request *req, u8 ghash[], int err)
{
	struct context *ctx = req->ctx;
	u8 __aligned(8) iv[BLOCK_SIZE];
	be128 __aligned(8) lengths;

	fill(ghash, BLOCK_SIZE);

	lengths.a = req->assoclen * 8;
	lengths.b = req->total * 8;

	fill(iv, IV_SIZE);

	scoped_guard() {
		if (req->assoclen)
			finish(ghash);
	}
	return err;
}
`,
	})
	if _, found := findFact(ff, "src.__aligned"); found {
		t.Errorf("an attribute macro was extracted as a function")
	}
	// The calls to fill sit in what the parser took for K&R parameter declarations
	// and were never seen as calls; that loss is the misparse's, and older than this
	// rule. The call inside the block is the one this rule must not take away.
	if crypt := mustFact(t, ff, "src.crypt"); !hasRelation(crypt, facts.RelCalls, "src.finish") {
		t.Errorf("src.crypt should keep its call to src.finish, got %+v", crypt.Relations)
	}
}

// A K&R definition lists bare identifiers and types them afterwards. Displaced by
// recovery it is still a definition.
func TestSwallowedKAndRDefinitionIsExtracted(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/x.c": `void first(int x)
{
  if (x) {
    x++;
}

int gcd (a, b)
int a;
int b;
{
    return b ? gcd (b, a % b) : a;
}
`,
	})
	mustFact(t, ff, "src.gcd")
}
