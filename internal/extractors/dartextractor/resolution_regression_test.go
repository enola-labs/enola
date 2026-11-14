package dartextractor

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestDartNamedConstructorTargets(t *testing.T) {
	all := walkSource(t, "lib/card.dart", `
class Card {
  Card.named();
  const Card.empty();
  factory Card.fromValue(Object value) => Card.named();
  static Card create() => Card.named();
}
Object run() => [Card.named(), Card.empty(), Card.fromValue(1), Card.create()];
`)
	all = resolveCallTargets(all)
	run := findFact(all, facts.KindSymbol, "lib.run")
	for _, target := range []string{"lib.Card.Card.named", "lib.Card.Card.empty", "lib.Card.Card.fromValue", "lib.Card.create"} {
		if !hasRelation(run, facts.RelCalls, target) {
			t.Errorf("missing call to %s: %+v", target, run)
		}
		if findFact(all, facts.KindSymbol, target) == nil {
			t.Errorf("call names missing declaration %s", target)
		}
	}
	// Exercise serialized target IDs as well as extractor names.
	store := facts.NewStore()
	store.Add(all...)
	var buf bytes.Buffer
	if err := store.WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&buf)
	for dec.More() {
		var f struct {
			Name      string `json:"name"`
			Relations []struct {
				Kind   string `json:"kind"`
				Target string `json:"target"`
				ID     string `json:"target_id"`
			} `json:"relations"`
		}
		if err := dec.Decode(&f); err != nil {
			t.Fatal(err)
		}
		if f.Name != "lib.run" {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls && r.ID == "" {
				t.Errorf("call %s has no target ID", r.Target)
			}
		}
	}
}

func TestDartQualifiedCallsKeepTheirOwner(t *testing.T) {
	all := walkSource(t, "lib/view.dart", `
class LocalView { LocalView.builder(); }
class Factory { static Object create() => 1; }
class Missing { }
Object run() => [ExternalView.builder(), LocalView.builder(), Missing.create(), Factory.create()];
`)
	all = resolveCallTargets(all)
	run := findFact(all, facts.KindSymbol, "lib.run")
	for _, target := range []string{"ExternalView.builder", "lib.LocalView.LocalView.builder", "Missing.create", "lib.Factory.create"} {
		if !hasRelation(run, facts.RelCalls, target) {
			t.Errorf("missing %s: %+v", target, run)
		}
	}
	if hasRelation(run, facts.RelCalls, "lib.Missing.create") {
		t.Error("invented canonical target without a declaration")
	}
}

func TestDartNamedConstructorAmbiguity(t *testing.T) {
	all := walkSource(t, "lib/a.dart", `class Card { Card.named(); }`)
	all = append(all, walkSource(t, "lib/b.dart", `class Card { Card.named(); }`)...)
	all = append(all, walkSource(t, "app/main.dart", `Object run() => Card.named();`)...)
	all = resolveCallTargets(all)
	if !hasRelation(findFact(all, facts.KindSymbol, "app.run"), facts.RelCalls, "Card.named") {
		t.Fatal("duplicate type names must remain unresolved")
	}
}

func TestDartNamedConstructorDoesNotCallField(t *testing.T) {
	all := walkSource(t, "lib/model/card.dart", `
class Card {
 final Object? value;
 Card.value(this.value);
}
`)
	all = append(all, walkSource(t, "lib/app/main.dart", `
import '../model/card.dart';
Object run() => Card.value(1);
`)...)
	all = resolveCallTargets(all)
	run := findFact(all, facts.KindSymbol, "lib/app.run")
	if !hasRelation(run, facts.RelCalls, "lib/model.Card.Card.value") {
		t.Fatalf("cross-file call lost named constructor: %+v", run)
	}
	if hasRelation(run, facts.RelCalls, "lib/model.Card.value") {
		t.Fatal("constructor call bound to instance field")
	}
}
