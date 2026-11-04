package cppextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestCppAutoBoundToAFactoryIsTypedByItsReturnType(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"CMakeLists.txt": "project(fx CXX)\n",
		"engine/engine.h": `#pragma once
#include <memory>
class Engine {
 public:
  int run();
  Engine* self();
};
class Registry {
 public:
  static Engine* make();
};
Engine* makeEngine();
std::unique_ptr<Engine> makeOwned();
`,
		"engine/engine.cpp": `#include "engine.h"
int Engine::run() { return 1; }
Engine* Engine::self() { return this; }
Engine* Registry::make() { return new Engine(); }
Engine* makeEngine() { return new Engine(); }
std::unique_ptr<Engine> makeOwned() { return std::make_unique<Engine>(); }
`,
		"api/api.cpp": `#include "../engine/engine.h"
int plain() { auto e = makeEngine(); return e->run(); }
int scoped() { auto e = Registry::make(); return e->run(); }
int method() { Engine* a = makeEngine(); auto b = a->self(); return b->run(); }
int owned() { auto o = makeOwned(); return o->run(); }
`,
	})
	calls := func(name string) []string {
		for _, f := range ff {
			if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, name) {
				var out []string
				for _, r := range f.Relations {
					if r.Kind == facts.RelCalls {
						out = append(out, r.Target)
					}
				}
				return out
			}
		}
		t.Fatalf("no symbol ending in %s", name)
		return nil
	}
	has := func(list []string, target string) bool {
		for _, c := range list {
			if c == target {
				return true
			}
		}
		return false
	}
	for _, name := range []string{"api.plain", "api.scoped", "api.method"} {
		if got := calls(name); !has(got, "engine.Engine::run") {
			t.Errorf("%s must call Engine::run; calls: %v", name, got)
		}
	}
	if got := calls("api.owned"); has(got, "engine.Engine::run") {
		t.Errorf("a unique_ptr<Engine> is not an Engine; calls: %v", got)
	}
	for _, f := range ff {
		if f.PropAny(propReturnType) != nil {
			t.Errorf("%s keeps %s, which must not leave the extractor", f.Name, propReturnType)
		}
	}
}
