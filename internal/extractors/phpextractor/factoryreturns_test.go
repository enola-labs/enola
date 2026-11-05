package phpextractor

import (
	"context"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestPHPLocalIsTypedByWhatItIsAssigned(t *testing.T) {
	files := map[string]string{
		"composer.json": `{"name":"acme/fx"}`,
		"src/Engine.php": `<?php
namespace Acme;
class Engine {
  public function run(): int { return 1; }
  public function session(): Session { return new Session(); }
  public function tuned(): static { return $this; }
}
`,
		"src/Session.php": `<?php
namespace Acme;
class Session { public function commit(): void {} }
`,
		"src/EngineFactory.php": `<?php
namespace Acme;
class EngineFactory {
  public static function make(): Engine { return new Engine(); }
  public function instance(): ?Engine { return new Engine(); }
}
`,
		"src/Api.php": `<?php
namespace Acme;
class Api {
  public function __construct(private EngineFactory $factory) {}
  public function viaStatic(): int { $e = EngineFactory::make(); return $e->run(); }
  public function viaNew(): int { $e = new Engine(); return $e->run(); }
  public function viaProperty(): int { $e = $this->factory->instance(); return $e->run(); }
  public function chained(): void { EngineFactory::make()->session()->commit(); }
  public function fluent(): int { $e = EngineFactory::make()->tuned(); return $e->run(); }
  public function conflicting(bool $f): int { $e = new Engine(); if ($f) { $e = new Session(); } return $e->run(); }
}
`,
	}
	dir := writeRepo(t, files)
	var rels []string
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	ff, err := New().Extract(context.Background(), dir, rels)
	if err != nil {
		t.Fatal(err)
	}
	calls := func(name string) facts.Fact {
		for _, f := range ff {
			if f.Kind == facts.KindSymbol && f.Name == name {
				return f
			}
		}
		t.Fatalf("no symbol %s", name)
		return facts.Fact{}
	}
	for _, name := range []string{"Acme\\Api::viaStatic", "Acme\\Api::viaNew", "Acme\\Api::viaProperty", "Acme\\Api::fluent"} {
		if f := calls(name); !f.HasRelation(facts.RelCalls, "Acme\\Engine::run") {
			t.Errorf("%s must call Engine::run; relations: %v", name, f.Relations)
		}
	}
	if f := calls("Acme\\Api::chained"); !f.HasRelation(facts.RelCalls, "Acme\\Session::commit") {
		t.Errorf("a chain is typed step by step; relations: %v", f.Relations)
	}
	if f := calls("Acme\\Api::conflicting"); f.HasRelation(facts.RelCalls, "Acme\\Engine::run") {
		t.Errorf("a local assigned two types has none; relations: %v", f.Relations)
	}
	for _, f := range ff {
		if f.Props[propReturnType] != nil {
			t.Errorf("%s keeps %s, which must not leave the extractor", f.Name, propReturnType)
		}
	}
}
