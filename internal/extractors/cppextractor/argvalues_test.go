package cppextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A name passed as a call argument is a function value only when it names a
// function. In C++ a local or a parameter of the enclosing function hides any
// function of that name, and a bare name never names another class's method.

const argValuesHeader = `struct dtime_tz_t { unsigned offset() const; unsigned bits; };
struct ListVector { static void SetListSize(int r, unsigned n); };
void Register(void (*cb)(int));
`

func TestCppArgumentLocalIsNotAFunction(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/include/types.hpp": argValuesHeader,
		"ext/map.cpp": "#include \"../src/include/types.hpp\"\n" + `
void MapFunction(int result) {
	unsigned offset = 0;
	ListVector::SetListSize(result, offset);
}
`,
	})
	fn := mustFact(t, ff, "ext.MapFunction")
	for _, r := range fn.Relations {
		if strings.HasSuffix(r.Target, "offset") {
			t.Errorf("the local offset is not a function, got %+v", r)
		}
	}
}

func TestCppArgumentParameterIsNotAFunction(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/include/types.hpp": argValuesHeader,
		"ext/approx.cpp": "#include \"../src/include/types.hpp\"\n" + `
void Export(int result, unsigned offset) {
	ListVector::SetListSize(result, offset);
}
`,
	})
	fn := mustFact(t, ff, "ext.Export")
	for _, r := range fn.Relations {
		if strings.HasSuffix(r.Target, "offset") {
			t.Errorf("the parameter offset is not a function, got %+v", r)
		}
	}
}

func TestCppBareArgumentDoesNotNameAnotherClassMethod(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/include/types.hpp": argValuesHeader,
		"ext/lambda.hpp":        `struct LambdaState { unsigned offset; void Execute(int result); };`,
		"ext/lambda.cpp": "#include \"lambda.hpp\"\n#include \"../src/include/types.hpp\"\n" + `
void LambdaState::Execute(int result) {
	ListVector::SetListSize(result, offset);
}
`,
	})
	fn := mustFact(t, ff, "ext.LambdaState::Execute")
	if hasRelation(fn, facts.RelCalls, "src/include.dtime_tz_t::offset") {
		t.Errorf("the member offset is not dtime_tz_t::offset, got %+v", fn.Relations)
	}
}

func TestCppBareArgumentStillNamesAFunction(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/include/types.hpp": argValuesHeader,
		"src/ready.hpp":         `inline void OnReady(int) {}`,
		"ext/setup.cpp": "#include \"../src/include/types.hpp\"\n#include \"../src/ready.hpp\"\n" + `
void Setup() {
	int unrelated = 0;
	Register(OnReady);
}
`,
	})
	fn := mustFact(t, ff, "ext.Setup")
	found := false
	for _, r := range fn.Relations {
		if r.Kind == facts.RelCalls && strings.HasSuffix(r.Target, ".OnReady") && !strings.HasPrefix(r.Target, "ext.") {
			found = true
		}
	}
	if !found {
		t.Errorf("Setup passes the function OnReady, got %+v", fn.Relations)
	}
}

// A qualified name in a file-scope initializer names its own declaration.
func TestCppQualifiedNameInFileScopeInitializer(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/include/radix.hpp": `struct RadixPartitioning { static unsigned NumberOfPartitions(unsigned bits); };`,
		"src/sort/tracker.cpp": "#include \"../include/radix.hpp\"\n" +
			`static const unsigned CAPACITY = RadixPartitioning::NumberOfPartitions(4);`,
	})
	found := false
	for _, f := range ff {
		if hasRelation(f, facts.RelCalls, "src/include.RadixPartitioning::NumberOfPartitions") {
			found = true
		}
	}
	if !found {
		t.Errorf("the initializer calls RadixPartitioning::NumberOfPartitions, got %v", factNames(ff))
	}
}
