package javaextractor

import (
	"slices"
	"testing"
)

func TestJavaCallOnAMethodsResultIsAnEdge(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main/java/com/acme/engine/Engine.java": `package com.acme.engine;
public class Engine {
  public int run() { return step(); }
  public int step() { return 1; }
  public Session session() { return new Session(); }
}
`,
		"src/main/java/com/acme/engine/Session.java": `package com.acme.engine;
public class Session { public void commit() {} }
`,
		"src/main/java/com/acme/engine/EngineFactory.java": `package com.acme.engine;
public class EngineFactory {
  public static Engine makeEngine() { return new Engine(); }
  public Engine instance() { return new Engine(); }
}
`,
		"src/main/java/com/acme/api/Api.java": `package com.acme.api;
import com.acme.engine.EngineFactory;
public class Api {
  private final EngineFactory factory = new EngineFactory();
  public int viaVar() { var e = EngineFactory.makeEngine(); return e.run(); }
  public int viaField() { var e = factory.instance(); return e.run(); }
  public void chained() { EngineFactory.makeEngine().session().commit(); }
  public int ownMethod() { var e = local(); return e.run(); }
  private com.acme.engine.Engine local() { return EngineFactory.makeEngine(); }
  public void library() { var s = String.valueOf(1); s.trim(); }
}
`,
	})
	run := "src/main/java/com/acme/engine.Engine.run"
	for _, name := range []string{"Api.viaVar", "Api.viaField", "Api.ownMethod"} {
		if f := javaSym(t, ff, name); !slices.Contains(javaCallTargets(f), run) {
			t.Errorf("%s must call Engine.run; calls: %v", name, javaCallTargets(f))
		}
	}
	if f := javaSym(t, ff, "Api.chained"); !slices.Contains(javaCallTargets(f), "src/main/java/com/acme/engine.Session.commit") {
		t.Errorf("a chain is typed step by step; calls: %v", javaCallTargets(f))
	}
	for _, f := range ff {
		if f.PropAny(propReturnType) != nil {
			t.Errorf("%s keeps %s, which must not leave the extractor", f.Name, propReturnType)
		}
	}
}
