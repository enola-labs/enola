package cppextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A definition whose signature a repository macro writes, `ENTRY(name, loader) {
// … }`, is a function the macro names by token pasting and a loader finds by
// symbol name: an entry point, never called by the macro's name.
func TestCppMacroWrittenDefinitionIsAnEntryPoint(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.hpp": "namespace db { class Loader; void Entry(); }\n" +
			"#define EXT_ENTRY(NAME, LOADER) void NAME##_init(db::Loader &LOADER)\n",
		"src/a.cpp": "#include \"a.hpp\"\nnamespace db { void Entry() {} }\n" +
			"extern \"C\" {\nEXT_ENTRY(demo, loader) { db::Entry(); }\n}\n" +
			"int plain(int x) { return x; }\n",
	})
	entry := mustFact(t, ff, "src.EXT_ENTRY")
	if entry.PropString(facts.PropFrameworkRoot) != rootMacro {
		t.Errorf("a macro-written definition should be an entry point, got %+v", entry.Props)
	}
	if !hasRelation(entry, facts.RelCalls, "src.db::Entry") {
		t.Errorf("its body's calls stay its own, got %+v", entry.Relations)
	}
	if plain := mustFact(t, ff, "src.plain"); plain.PropString(facts.PropFrameworkRoot) != "" {
		t.Errorf("an ordinary definition is no entry point, got %+v", plain.Props)
	}
}

// Only a macro the repository defines: an unknown name with no return type is
// left as it was.
func TestCppUnknownMacroDefinitionIsNotAnEntryPoint(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"src/a.cpp": "UNKNOWN_ENTRY(demo, loader) { }\n",
	})
	for _, f := range ff {
		if f.PropString(facts.PropFrameworkRoot) != "" {
			t.Errorf("%s has no macro definition in the repository, got %+v", f.Name, f.Props)
		}
	}
}
