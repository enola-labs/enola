package scalaextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestScalaLocalValIsALocalTypedByWhatItHolds(t *testing.T) {
	ff := runPasses(t, map[string]string{
		"src/main/scala/acme/engine/Engine.scala": `package acme.engine
class Engine {
  def run(): Int = 1
  def session(): Session = new Session()
}
class Session { def commit(): Unit = () }
`,
		"src/main/scala/acme/engine/Factory.scala": `package acme.engine
object Factory {
  def makeEngine(): Engine = new Engine()
}
`,
		"src/main/scala/acme/api/Api.scala": `package acme.api
import acme.engine.{Engine, Factory}
class Api {
  def viaFactory(): Int = { val e = Factory.makeEngine(); e.run() }
  def viaNew(): Int = { val e = new Engine(); e.run() }
  def ascribed(): Int = { val e: Engine = Factory.makeEngine(); e.run() }
  def chained(): Unit = { Factory.makeEngine().session().commit() }
  private def local(): Engine = new Engine()
  def ownDef(): Int = { val e = local(); e.run() }
  def withLocalClass(): Int = {
    class Helper { val inner = 1 }
    new Helper().inner
  }
}
`,
	}, nil)
	byName := map[string]facts.Fact{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			byName[f.Name] = f
		}
	}
	const pre = "src/main/scala/acme/api.Api."
	for _, name := range []string{"viaFactory", "viaNew", "ascribed", "ownDef"} {
		f, ok := byName[pre+name]
		if !ok {
			t.Fatalf("no symbol %s", pre+name)
		}
		if !f.HasRelation(facts.RelCalls, "src/main/scala/acme/engine.Engine.run") {
			t.Errorf("%s must call Engine.run; relations: %v", name, f.Relations)
		}
	}
	if f := byName[pre+"viaFactory"]; !f.HasRelation(facts.RelCalls, "src/main/scala/acme/engine.Factory.makeEngine") {
		t.Errorf("a local's initializer is the def's call; relations: %v", f.Relations)
	}
	if f := byName[pre+"chained"]; !f.HasRelation(facts.RelCalls, "src/main/scala/acme/engine.Session.commit") {
		t.Errorf("a chain is typed step by step; relations: %v", f.Relations)
	}
	if _, ok := byName[pre+"e"]; ok {
		t.Errorf("a val in a def body is a local, not a member of Api")
	}
	if _, ok := byName["src/main/scala/acme/api.Api.Helper.inner"]; !ok {
		t.Errorf("a val of a class declared in a def is that class's member")
	}
	for _, f := range ff {
		if f.PropAny(propReturnType) != nil {
			t.Errorf("%s keeps %s, which must not leave the extractor", f.Name, propReturnType)
		}
	}
}
