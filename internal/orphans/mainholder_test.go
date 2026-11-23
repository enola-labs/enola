package orphans

import "testing"

// A JVM class whose static main the launcher runs is an entry point as a whole,
// as its main is: nothing in the program names it. A class with an instance
// method called main, or a Kotlin or Go symbol of that name, earns nothing.
func TestJavaClassHoldingStaticMainIsEntryPoint(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"tool.Generator","file":"tool/Generator.java","props":{"symbol_kind":"class","language":"java"},"relations":[{"kind":"declares","target":"tool"}]}
{"kind":"symbol","name":"tool.Generator.main","file":"tool/Generator.java","props":{"symbol_kind":"method","language":"java","static":true},"relations":[{"kind":"declares","target":"tool"}]}
{"kind":"symbol","name":"tool.Runner","file":"tool/Runner.java","props":{"symbol_kind":"class","language":"java"},"relations":[{"kind":"declares","target":"tool"}]}
{"kind":"symbol","name":"tool.Runner.main","file":"tool/Runner.java","props":{"symbol_kind":"method","language":"java"},"relations":[{"kind":"declares","target":"tool"}]}
{"kind":"symbol","name":"tool.Dead","file":"tool/Dead.java","props":{"symbol_kind":"class","language":"java"},"relations":[{"kind":"declares","target":"tool"}]}
`
	syms, refs := collectFromJSONL(t, jsonl)
	got := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	if _, ok := got["tool.Generator"]; ok {
		t.Error("a class declaring a static main is an entry point and should be excluded")
	}
	for _, n := range []string{"tool.Runner", "tool.Dead"} {
		if _, ok := got[n]; !ok {
			t.Errorf("%s should still be reported", n)
		}
	}
	in := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all", IncludeEntrypoints: true}))
	if _, ok := in["tool.Generator"]; !ok {
		t.Error("include_entrypoints should report the main class")
	}
}
