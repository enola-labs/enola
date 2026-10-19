package scalaextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A type application standing alone as a receiver, `tradingFsm[IO].apply(x)`,
// still calls tradingFsm: a value cannot take type arguments.
func TestTypeApplicationReceiverIsACall(t *testing.T) {
	src := `package trading.trace

object Main:
  def resources = tradingFsm[IO].apply(tracer)
  def plain = Ember.default[IO](port)
`
	ff := extractAST(t, "trace/Main.scala", src)
	res := findFact(t, ff, "trace.Main.resources")
	if !hasRelation(res, facts.RelCalls, "tradingFsm") {
		t.Errorf("missing call to tradingFsm; got %v", res.Relations)
	}
	plain := findFact(t, ff, "trace.Main.plain")
	n := 0
	for _, r := range plain.Relations {
		if r.Kind == facts.RelCalls && r.Target == "Ember.default" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("`f[T](x)` must emit its call once, got %d in %v", n, plain.Relations)
	}
}

// A capitalized identifier used as a value or a case pattern names the object
// this file declares or imports. One resolved neither way (None) emits nothing.
func TestObjectValueReferencesAreNamed(t *testing.T) {
	src := `package gitbucket.core.api

import gitbucket.core.model.Role

object ApiBranchProtectionResponse {
  sealed class EnforcementLevel(val name: String)
  case object Off extends EnforcementLevel("off")
  case object Everyone extends EnforcementLevel("everyone")

  object EnforcementLevel {
    def apply(enabled: Boolean): EnforcementLevel =
      if (enabled) Everyone else Off
  }

  def parse(s: String): Option[EnforcementLevel] = s match {
    case "everyone" => Some(Everyone)
    case _          => None
  }

  def role = Role
}
`
	ff := extractAST(t, "core/api/ApiBranchProtectionResponse.scala", src)
	apply := findFact(t, ff, "core/api.ApiBranchProtectionResponse.EnforcementLevel.apply")
	for _, obj := range []string{"core/api.ApiBranchProtectionResponse.Everyone", "core/api.ApiBranchProtectionResponse.Off"} {
		if !hasRelation(apply, facts.RelNames, obj) {
			t.Errorf("apply: missing names %s; got %v", obj, apply.Relations)
		}
	}
	parse := findFact(t, ff, "core/api.ApiBranchProtectionResponse.parse")
	if !hasRelation(parse, facts.RelNames, "core/api.ApiBranchProtectionResponse.Everyone") {
		t.Errorf("parse: missing names Everyone; got %v", parse.Relations)
	}
	for _, r := range parse.Relations {
		if r.Kind == facts.RelNames && r.Target == "None" {
			t.Errorf("parse: None is not declared or imported here and must not be named")
		}
	}
	role := findFact(t, ff, "core/api.ApiBranchProtectionResponse.role")
	if !hasRelation(role, facts.RelNames, "gitbucket.core.model.Role") {
		t.Errorf("role: missing names of the imported object; got %v", role.Relations)
	}
}

// The grammar can attach a type application to a whole infix chain:
// `new A[D](f) + B[C]()` parses with `new A[D](f) + B` as the generic callee.
// Its edges must still be walked.
func TestTypeApplicationOverAnInfixChainKeepsItsEdges(t *testing.T) {
	ff := extractAST(t, "a/A.scala", "package p\nobject A {\n  val j = new CustomSerializer[Date](format => JString(\"y\")) + FieldSerializer[ApiUser]()\n}\n")
	j := findFact(t, ff, "a.A.j")
	for _, typ := range []string{"CustomSerializer", "JString"} {
		if !hasRelation(j, facts.RelInstantiates, typ) {
			t.Errorf("missing instantiates %s; got %v", typ, j.Relations)
		}
	}
}

// A chain of type-applied calls with block arguments, `.a[T] { … }.b[U] { … }`,
// is walked once: re-walking the generic callee had emitted every earlier block's
// symbols again for each later link.
func TestTypeAppliedCallChainEmitsEachSymbolOnce(t *testing.T) {
	src := `package p

object H {
  def handler = builder
    .handleAll[A] { rs =>
      val first = rs.size
      first
    }
    .handleAll[B] { rs =>
      val second = rs.size
      second
    }
    .handleAll[C] { rs => rs }
}
`
	ff := extractAST(t, "p/H.scala", src)
	seen := map[string]int{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			seen[f.Name]++
		}
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("%s emitted %d times", name, n)
		}
	}
}
