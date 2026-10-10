package scalaextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/extractors/ioclosure"
	"github.com/enola-labs/enola/internal/facts"
)

// runPasses runs both extraction passes over a set of in-memory files, which is
// what a real repository does and what the walker alone cannot: same-package
// resolution is decidable only once every file has been merged.
func runPasses(t *testing.T, files map[string]string, crossLang map[string]string) []facts.Fact {
	t.Helper()
	var all []facts.Fact
	filePkg := map[string]string{}
	// Deterministic order, mirroring the engine's file-ordered merge.
	for _, name := range sortedKeys(files) {
		ff, pkg := extractFileASTFull([]byte(files[name]), name, crossLang)
		all = append(all, ff...)
		if pkg != "" {
			filePkg[name] = pkg
		}
	}
	if crossLang == nil {
		crossLang = map[string]string{}
	}
	// Same order as Extract: the I/O closure walks the call graph, so it must run
	// after targets are canonical or every cross-file edge is still a bare name and
	// the propagation stops at the first hop.
	canonicalizeTargets(all, crossLang, filePkg)
	ioclosure.Propagate(all)
	return all
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TestSamePackageResolvedOnlyWhenDeclared is the test for the decision that
// separates Scala's resolution from Java's. Java can qualify a bare reference with
// the file's package because its imports are explicit. Scala cannot: it
// auto-imports scala.*, java.lang.* and scala.Predef.*, so a bare name is as likely
// to be stdlib as same-package. The two references below are syntactically
// identical and must resolve differently — one to a sibling type, one not at all.
func TestSamePackageResolvedOnlyWhenDeclared(t *testing.T) {
	ff := runPasses(t, map[string]string{
		"src/Base.scala": `package com.example.app

trait Base
`,
		"src/Service.scala": `package com.example.app

class Service extends Base with Ordering
`,
	}, nil)

	svc := findFact(t, ff, "src.Service")

	// Declared in the same package -> canonical fact name.
	if !hasRelation(svc, facts.RelImplements, "src.Base") {
		t.Errorf("same-package Base should resolve to src.Base; got %+v", svc.Relations)
	}
	// NOT declared anywhere -> left bare. A fabricated `com.example.app.Ordering`
	// would materialize a phantom node, because the graph is name-keyed.
	if !hasRelation(svc, facts.RelImplements, "Ordering") {
		t.Errorf("undeclared Ordering should stay bare; got %+v", svc.Relations)
	}
	for _, r := range svc.Relations {
		if r.Target == "com.example.app.Ordering" {
			t.Errorf("fabricated a same-package FQN for a stdlib type: %+v", svc.Relations)
		}
	}
}

// TestCrossLanguageTypeResolves pins the edge that keeps a mixed JVM repository
// from being two disconnected graphs. The Java type's facts belong to a different
// extractor and are not in this slice, so the target cannot be looked up — but the
// package index knows the directory and every JVM extractor names a type
// "<dir>.<Type>", so the canonical name is derivable.
func TestCrossLanguageTypeResolves(t *testing.T) {
	crossLang := map[string]string{
		"com.example.legacy": "core/src/main/java/com/example/legacy",
	}
	ff := runPasses(t, map[string]string{
		"app/Service.scala": `package com.example.app

import com.example.legacy.LegacyId

class Service {
  val id = new LegacyId(1L)
}
`,
	}, crossLang)

	f := findFact(t, ff, "app.Service.id")
	want := "core/src/main/java/com/example/legacy.LegacyId"
	if !hasRelation(f, facts.RelInstantiates, want) {
		t.Errorf("cross-language instantiate should resolve to %s; got %+v", want, f.Relations)
	}
}

// TestMemberImportIsNotRewrittenToAType guards the gate on rule 3: only a
// capitalized last segment is treated as a type. A member reference resolved as one
// would invent a type nobody declared.
func TestMemberImportIsNotRewrittenToAType(t *testing.T) {
	crossLang := map[string]string{"com.example.core": "core/src/main/scala/com/example/core"}
	ff := runPasses(t, map[string]string{
		"app/Use.scala": `package com.example.app

import com.example.core.Registry.cache

class Use extends cache
`,
	}, crossLang)

	f := findFact(t, ff, "app.Use")
	for _, r := range f.Relations {
		if r.Kind == facts.RelImplements && r.Target == "core/src/main/scala/com/example/core.cache" {
			t.Errorf("lowercase member was rewritten as a type: %+v", f.Relations)
		}
	}
}

// TestImportsPromotedToInternal pins the dependency half of pass 2: an import that
// resolves inside the repository is reclassified and repointed at the declaring
// directory, so module-level coupling is visible. A stdlib classification must
// survive even when a repository declares a colliding package.
func TestImportsPromotedToInternal(t *testing.T) {
	ff := runPasses(t, map[string]string{
		"core/Base.scala": `package com.example.core

trait Base
`,
		"app/Service.scala": `package com.example.app

import com.example.core.Base
import scala.concurrent.Future
import org.thirdparty.Widget

class Service extends Base
`,
	}, nil)

	got := map[string]string{}
	target := map[string]string{}
	for _, f := range ff {
		if f.Kind != facts.KindDependency {
			continue
		}
		imp, _ := f.Props["import"].(string)
		src, _ := f.Props[facts.PropSource].(string)
		got[imp] = src
		for _, r := range f.Relations {
			if r.Kind == facts.RelImports {
				target[imp] = r.Target
			}
		}
	}

	if got["com.example.core.Base"] != "internal" {
		t.Errorf("own-repo import: source = %q, want internal", got["com.example.core.Base"])
	}
	if target["com.example.core.Base"] != "core" {
		t.Errorf("own-repo import: target = %q, want the declaring dir 'core'", target["com.example.core.Base"])
	}
	if got["scala.concurrent.Future"] != "stdlib" {
		t.Errorf("stdlib import: source = %q, want stdlib", got["scala.concurrent.Future"])
	}
	if got["org.thirdparty.Widget"] != "external" {
		t.Errorf("third-party import: source = %q, want external", got["org.thirdparty.Widget"])
	}
}

// TestCompanionObjectDoesNotFightItsClass pins the index tie-break for the single
// most common shape in Scala: a class and its companion object share one FQN, and a
// name-keyed graph merges them. Whichever wins must be a function of file order
// rather than of map iteration, or the snapshot stops being reproducible.
func TestCompanionObjectDoesNotFightItsClass(t *testing.T) {
	files := map[string]string{
		"src/User.scala": `package com.example.core

class User
object User
`,
		"src/Use.scala": `package com.example.core

class Use {
  val u = new User()
}
`,
	}
	first := runPasses(t, files, nil)
	for i := 0; i < 5; i++ {
		again := runPasses(t, files, nil)
		if len(again) != len(first) {
			t.Fatalf("fact count varies between runs: %d vs %d", len(again), len(first))
		}
		for j := range first {
			if first[j].Name != again[j].Name {
				t.Fatalf("fact order varies between runs at %d: %q vs %q", j, first[j].Name, again[j].Name)
			}
			if len(first[j].Relations) != len(again[j].Relations) {
				t.Fatalf("relations vary between runs for %s", first[j].Name)
			}
			for k := range first[j].Relations {
				if first[j].Relations[k] != again[j].Relations[k] {
					t.Fatalf("relation target varies between runs for %s: %v vs %v",
						first[j].Name, first[j].Relations[k], again[j].Relations[k])
				}
			}
		}
	}
	if f := findFact(t, first, "src.Use.u"); !hasRelation(f, facts.RelInstantiates, "src.User") {
		t.Errorf("companion pair should still resolve; got %+v", f.Relations)
	}
}
