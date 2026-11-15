package dartextractor

import (
	"github.com/enola-labs/enola/internal/facts"

	"bytes"
	"slices"
	"testing"
)

func TestDartScopeReviewShadowing(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `import 'package:http/http.dart'; class Owner { void save(List items) { items.forEach((save) => save()); { final save=callback; save(); } for(final item in items) { save(items); } httpClient.get('/'); } }`})
	f := findFact(all, facts.KindSymbol, "lib.Owner.save")
	if f == nil || !f.PropBool("recursive_self") || !slices.Contains(f.Props["calls_in_loop"].([]string), "lib.Owner.save") {
		t.Fatalf("outer binding lost: %+v", f)
	}
	if !hasReviewCall(f, "lib.Owner.save") || !hasReviewCall(f, "(save).call") {
		t.Fatalf("scope calls: %+v", f)
	}
}
func hasReviewCall(f *facts.Fact, target string) bool {
	if f != nil {
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls && r.Target == target {
				return true
			}
		}
	}
	return false
}
func TestDartScopeReviewTypeVisibility(t *testing.T) {
	for _, tc := range []struct{ file, src, target string }{
		{"lib/a.dart", `import 'package:flutter/widgets.dart'; void run(Image img) { img.resolve(); }`, "ui.Image.resolve"},
		{"lib/a.dart", `import 'dart:ui' as ui; void run(ui.Image img) { img.resolve(); }`, "ui.Image.resolve"},
		{"lib/a.dart", `import 'package:foreign/api.dart'; void run(Foo foo) { foo.resolve(); }`, "Foo.resolve"},
	} {
		t.Run(tc.src, func(t *testing.T) {
			all := dartReviewFacts(t, map[string]string{tc.file: tc.src, "ui/image.dart": `class Image { void resolve() {} }`, "foo.dart": `class Foo { void resolve() {} }`})
			store := facts.NewStore()
			store.Add(all...)
			var buf bytes.Buffer
			if err := store.WriteJSONL(&buf); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(buf.Bytes(), []byte(`"kind":"calls","target":"`+tc.target+`"`)) {
				t.Fatalf("serialization can bind unrelated target: %s", buf.String())
			}
			if f := findFact(all, facts.KindSymbol, "lib.run"); hasReviewCall(f, tc.target) {
				t.Fatalf("unimported class bound: %+v", f)
			}
		})
	}
}
func TestDartScopeReviewPartLibrary(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/host.dart":  `import 'api.dart' as api; part 'piece.dart'; void hostLoad() {}`,
		"lib/piece.dart": `part of 'host.dart'; void run(api.Client c) { c.fetch(); hostLoad(); }`,
		"lib/api.dart":   `class Client { void fetch() {} }`,
		"other/a.dart":   `void hostLoad() {}`,
	})
	f := findFact(all, facts.KindSymbol, "lib.run")
	if !hasReviewCall(f, "lib.Client.fetch") || !hasReviewCall(f, "lib.hostLoad") {
		t.Fatalf("part lost scope: %+v", f)
	}
}
func TestDartScopeReviewRetrofitUntyped(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `import 'package:retrofit/retrofit.dart'; @RestApi() abstract class Api { @GET('/u') getUser(); }`})
	f := findFact(all, facts.KindSymbol, "lib.Api.getUser")
	if f == nil || !f.PropBool("abstract_member") || !f.PropBool("performs_io") {
		t.Fatalf("retrofit IO: %+v", f)
	}
}
func TestDartScopeReviewCascadeWrite(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `import 'package:http/http.dart'; class Owner { get value => httpClient.get('/'); void run(other) { other..value = 3; } }`})
	f := findFact(all, facts.KindSymbol, "lib.Owner.run")
	if hasReviewCall(f, "lib.Owner.value") || f.PropBool("performs_io") {
		t.Fatalf("cascade write read own getter: %+v", f)
	}
}
func TestDartScopeReviewConstructorLoop(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `class Item { Item(x); } class Repo { void run(items) { for(final x in items) { Item(x); } } }`})
	f := findFact(all, facts.KindSymbol, "lib.Repo.run")
	if f == nil || !slices.Contains(f.Props["calls_in_loop"].([]string), "Item") || slices.Contains(f.Props["calls_in_loop"].([]string), "lib.Repo.Item") {
		t.Fatalf("constructor metrics: %+v", f)
	}
}
func TestDartScopeReviewParenthesizedIO(t *testing.T) {
	for _, call := range []string{`(await openDb()).query('x')`, `(widget.db).rawQuery('x')`} {
		all := dartReviewFacts(t, map[string]string{"lib/a.dart": `import 'package:sqflite/sqflite.dart'; Future run() async { ` + call + `; }`})
		f := findFact(all, facts.KindSymbol, "lib.run")
		if f == nil || !f.PropBool("io_direct") || !f.PropBool("performs_io") {
			t.Fatalf("parenthesized IO %s: %+v", call, f)
		}
	}
}

func TestDartScopeReviewExplicitThisWithPrefixCollision(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"ui/image.dart": `import 'dart:ui' as ui; class Image { void save() { this.save(); } }`})
	f := findFact(all, facts.KindSymbol, "ui.Image.save")
	if !hasReviewCall(f, "ui.Image.save") || !f.PropBool("recursive_self") {
		t.Fatalf("this lost to prefix: %+v", f)
	}
}
func TestDartScopeReviewNestedBindings(t *testing.T) {
	for _, body := range []string{
		`items.forEach((save) => save()); save();`,
		`{ final save=cb; save(); } save();`,
		`for(final save in items) { save(); } save();`,
		`try {} catch(save) { save(); } save();`,
		`void nested(save) { save(); } save();`,
	} {
		all := dartReviewFacts(t, map[string]string{"lib/a.dart": `import 'package:http/http.dart'; class Owner { void save() { httpClient.get('/'); } void run(items) { ` + body + ` } }`})
		f := findFact(all, facts.KindSymbol, "lib.Owner.run")
		if !hasReviewCall(f, "lib.Owner.save") || !hasReviewCall(f, "(save).call") || !f.PropBool("performs_io") {
			t.Errorf("nested scope %s: %+v", body, f)
		}
	}
}

func TestDartScopeReviewClosureBindingDoesNotCallOuterMethod(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{"lib/a.dart": `import 'package:http/http.dart'; class Owner { void save() { httpClient.get('/'); } void run(items) { items.forEach((save) => save()); } }`})
	f := findFact(all, facts.KindSymbol, "lib.Owner.run")
	if hasReviewCall(f, "lib.Owner.save") || !hasReviewCall(f, "(save).call") || f.PropBool("performs_io") {
		t.Fatalf("closure binding called outer method: %+v", f)
	}
}
