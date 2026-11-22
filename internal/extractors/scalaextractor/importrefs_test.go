package scalaextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func namesTargets(f facts.Fact) []string {
	var out []string
	for _, r := range f.Relations {
		if r.Kind == facts.RelNames {
			out = append(out, r.Target)
		}
	}
	return out
}

func fileRefFor(ff []facts.Fact, file string) *facts.Fact {
	for i := range ff {
		if ff[i].Kind == facts.KindFileRef && ff[i].File == file {
			return &ff[i]
		}
	}
	return nil
}

// `import Parsers._` brings an object's members into scope, and the members are then
// used unqualified, so nothing else names the object: the import is the reference.
// Inside a type the edge hangs on that type; at file scope on the file's reference
// fact. A package wildcard (lower case) and a stdlib object name nothing here.
func TestWildcardObjectImportNamesTheObject(t *testing.T) {
	ff := runPasses(t, map[string]string{
		"core/parsing/Parsers.scala": `package org.ex.core.parsing

object Parsers {
  def specialized = Seq(1)
}
`,
		"core/util/Constants.scala": `package org.ex.core.util

object Constants {
  val Limit = 3
}
`,
		"core/parsing/HeaderParser.scala": `package org.ex.core.parsing

import org.ex
import ex.core.util.Constants._
import scala.concurrent.ExecutionContext.Implicits._
import org.ex.core.util._

object HeaderParser {
  import Parsers._

  def prime = insert(specialized)
}
`,
	}, nil)

	hp := findFact(t, ff, "core/parsing.HeaderParser")
	if !hasRelation(hp, facts.RelNames, "core/parsing.Parsers") {
		t.Errorf("object importing Parsers._ must name it; names = %v", namesTargets(hp))
	}
	ref := fileRefFor(ff, "core/parsing/HeaderParser.scala")
	if ref == nil {
		t.Fatalf("no file reference fact for the file-scope object import")
	}
	if !hasRelation(*ref, facts.RelNames, "core/util.Constants") {
		t.Errorf("file-scope `import ex.core.util.Constants._` must name the object through the aliased root; names = %v", namesTargets(*ref))
	}
	for _, target := range namesTargets(*ref) {
		if target != "core/util.Constants" {
			t.Errorf("unexpected file-scope names edge %q (package wildcard or stdlib object)", target)
		}
	}
	// A file with no object import keeps no empty reference fact.
	if r := fileRefFor(ff, "core/parsing/Parsers.scala"); r != nil {
		t.Errorf("unexpected file reference fact %+v", *r)
	}
}

// A selector import and a member import reach through the object as well.
func TestSelectorAndMemberObjectImportsNameTheObject(t *testing.T) {
	ff := runPasses(t, map[string]string{
		"a/Registry.scala": `package org.ex.a

object Registry {
  def next = 1
  def last = 2
}
`,
		"b/User.scala": `package org.ex.b

import org.ex.a.Registry.{next, last => l}

class User {
  def id = next + l
}

class Other {
  import org.ex.a.Registry.next
  def id = next
}
`,
	}, nil)
	ref := fileRefFor(ff, "b/User.scala")
	if ref == nil || !hasRelation(*ref, facts.RelNames, "a.Registry") {
		t.Errorf("selector import must name Registry; got %v", ref)
	}
	other := findFact(t, ff, "b.Other")
	if !hasRelation(other, facts.RelNames, "a.Registry") {
		t.Errorf("member import must name Registry; names = %v", namesTargets(other))
	}
}

// An object import that resolves to nothing this repository declares adds no edge:
// a short-name match on an external object would vouch for an unrelated local one.
func TestUnresolvedObjectImportNamesNothing(t *testing.T) {
	ff := runPasses(t, map[string]string{
		"a/Main.scala": `package org.ex.a

import cats.effect.IO._

object Main {
  import Implicits._
  def run = 1
}
`,
	}, nil)
	if ref := fileRefFor(ff, "a/Main.scala"); ref != nil {
		t.Errorf("unresolved object import produced %+v", *ref)
	}
	main := findFact(t, ff, "a.Main")
	if got := namesTargets(main); len(got) != 0 {
		t.Errorf("unresolved object import named %v", got)
	}
}
