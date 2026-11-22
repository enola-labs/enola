package scalaextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// `Props[util.Logger]()` creates the actor from its type argument alone; the class
// is named nowhere else. The type arguments of a called type application, of one
// standing alone (`classOf[T]`), and a qualified type relative to the file's package
// all name their type.
func TestTypeArgumentsNameTheirType(t *testing.T) {
	ff := runPasses(t, map[string]string{
		"impl/util/package.scala": `package org.ex.impl

package object util {
  def install(system: ActorSystem) =
    system.actorOf(Props[util.EventLogger]().withDeploy(Deploy.local))
  def cls = classOf[Codec]
  def decode = parse[Payload, util.Header](1)
}

package util {
  class EventLogger
  class Codec
  class Payload
  class Header
}
`,
	}, nil)
	install := findFact(t, ff, "impl/util.util.install")
	if !hasRelation(install, facts.RelNames, "impl/util.EventLogger") {
		t.Errorf("Props[util.EventLogger]() must name EventLogger; names = %v", namesTargets(install))
	}
	cls := findFact(t, ff, "impl/util.util.cls")
	if !hasRelation(cls, facts.RelNames, "impl/util.Codec") {
		t.Errorf("classOf[Codec] must name Codec; names = %v", namesTargets(cls))
	}
	decode := findFact(t, ff, "impl/util.util.decode")
	for _, want := range []string{"impl/util.Payload", "impl/util.Header"} {
		if !hasRelation(decode, facts.RelNames, want) {
			t.Errorf("parse[Payload, util.Header](1) must name %s; names = %v", want, namesTargets(decode))
		}
	}
}

// A qualified type that resolves to nothing declared here falls back to what its
// last segment resolved to before, so an external `mutable.Map` never leaves as a
// dotted edge that a short-name match would bind to an unrelated local `Map`.
func TestQualifiedTypeFallsBackToBareResolution(t *testing.T) {
	ff := runPasses(t, map[string]string{
		"a/Store.scala": `package org.ex.a

class Store {
  val m: mutable.Map[String, Int] = null
  val h: util.Header = null
}
`,
		"a/util/Header.scala": `package org.ex.a.util

class Header
`,
	}, nil)
	if got := namesTargets(findFact(t, ff, "a.Store.m")); len(got) != 0 {
		t.Errorf("external qualified type named %v", got)
	}
	if h := findFact(t, ff, "a.Store.h"); !hasRelation(h, facts.RelNames, "a/util.Header") {
		t.Errorf("util.Header must resolve relative to the package; names = %v", namesTargets(h))
	}
}
