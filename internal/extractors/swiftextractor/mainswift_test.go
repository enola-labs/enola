package swiftextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// The top-level statements of main.swift are the program's entry point, so their
// calls belong to a main symbol. In any other file they stay on the file.
func TestMainSwiftTopLevelIsMain(t *testing.T) {
	src := "let r = Runner()\nr.run()\nsetup()\n"
	ff := extractFileAST([]byte(src), "Sources/App/main.swift", false)
	main, ok := findFact(ff, "Sources/App.main")
	if !ok || main.Kind != facts.KindSymbol || main.PropString("symbol_kind") != facts.SymbolFunc {
		t.Fatalf("no main function symbol: %v", factNames(ff))
	}
	if !hasRelation(main, facts.RelCalls, "Sources/App.setup") {
		t.Errorf("main calls %v, want setup", main.Relations)
	}
	other := extractFileAST([]byte(src), "Sources/App/script.swift", false)
	if _, ok := findFact(other, "Sources/App.main"); ok {
		t.Error("a file not named main.swift has no main")
	}
	if len(findFactByKind(other, facts.KindFileRef)) != 1 {
		t.Error("the file keeps its top-level calls")
	}
}
