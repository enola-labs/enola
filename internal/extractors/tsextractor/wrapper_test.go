package tsextractor

import (
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// symbolsIn returns the names of the symbols declared in a file, sorted.
func symbolsIn(ff []facts.Fact, file string) []string {
	var out []string
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.File == file {
			out = append(out, f.Name)
		}
	}
	slices.Sort(out)
	return out
}

// A script written for a <script> tag keeps its declarations in a function it calls
// at once. Read as that function's locals, the file had no symbols at all. SIDES is a
// plain variable of the wrapper and is not one of them.
func TestWrapperFunctionDeclarationsAreFileLevel(t *testing.T) {
	cases := map[string]string{
		"called through parentheses": `(function (global) {
  'use strict';
  const SIDES = 6;
  function roll() { return SIDES; }
  class Game { start() { return roll(); } }
  const pick = (xs) => xs[0];
  global.Game = Game;
})(window);`,
		"called inside the parentheses": `(function () {
  const SIDES = 6;
  function roll() { return SIDES; }
  class Game { start() { return roll(); } }
  const pick = (xs) => xs[0];
}());`,
		"prefixed with a bang": `!function () {
  const SIDES = 6;
  function roll() { return SIDES; }
  class Game { start() { return roll(); } }
  const pick = (xs) => xs[0];
}();`,
		"an arrow function": `(() => {
  const SIDES = 6;
  function roll() { return SIDES; }
  class Game { start() { return roll(); } }
  const pick = (xs) => xs[0];
})();`,
	}
	want := "js.Game,js.Game.start,js.pick,js.roll"
	for name, src := range cases {
		ff := extractTSRepo(t, map[string]string{"js/engine.js": src, "js/keep.ts": "export const x = 1\n"})
		if got := strings.Join(symbolsIn(ff, "js/engine.js"), ","); got != want {
			t.Errorf("%s: symbols = %q, want %q", name, got, want)
		}
		roll := tsSym(t, ff, "js.roll")
		if roll.Props["exported"] != false {
			t.Errorf("%s: roll is exported = %v; a wrapper exists to keep its contents private", name, roll.Props["exported"])
		}
	}
}

// Only a function that is called where it is written is a file's top level. A function
// handed to something else runs later, if at all, and a function's own locals stay local.
// That includes the factory a universal-module wrapper is handed, which is the form
// checked-in library builds come in.
func TestOnlyImmediatelyCalledFunctionsAreWrappers(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{"js/app.js": `document.addEventListener('DOMContentLoaded', function () {
  function onReady() {}
  onReady();
});

function outer() {
  function inner() {}
  (function () { function deeper() {} })();
  return inner;
}

setup(function () { function later() {} });

(function (root, factory) {
  root.Lib = factory();
})(this, function () {
  function insideFactory() {}
  return insideFactory;
});
`, "js/keep.ts": "export const x = 1\n"})
	if got := strings.Join(symbolsIn(ff, "js/app.js"), ","); got != "js.outer" {
		t.Errorf("symbols = %q, want only js.outer", got)
	}
}

// A wrapper inside a wrapper is followed, as a bundle of wrapped files needs, and a
// module is read exactly as before: nothing in it is declared twice.
func TestWrapperNestingAndModulesUnchanged(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{
		"js/bundle.js": `(function () {
  (function () { function first() {} })();
  (function () { (function () { function tooDeep() {} })(); function second() {} })();
})();`,
		"src/mod.ts": "export function a() {}\nconst b = () => 1\nexport class C {}\n",
	})
	if got := strings.Join(symbolsIn(ff, "js/bundle.js"), ","); got != "js.first,js.second" {
		t.Errorf("bundle symbols = %q, want js.first,js.second", got)
	}
	if got := strings.Join(symbolsIn(ff, "src/mod.ts"), ","); got != "src.C,src.a,src.b" {
		t.Errorf("module symbols = %q, want src.C,src.a,src.b", got)
	}
}

// A wrapper's variables are its own business unless they are code: a function written
// as a value, or an object carrying functions. The names it gives to globals are not.
func TestWrapperKeepsCodeAndDropsPlainVariables(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{"js/admin.js": `(function ($) {
  var __ = wp.i18n.__;
  var $doc = $(document), count = 0;
  var legacy = function () { return count; };
  var api = { init: function () { legacy(); }, reset() {} };
  window.adminApi = api;
})(jQuery);`, "js/keep.ts": "export const x = 1\n"})
	got := strings.Join(symbolsIn(ff, "js/admin.js"), ",")
	if got != "js.api,js.api.init,js.api.reset,js.legacy" {
		t.Errorf("symbols = %q, want the function, and the object with its members", got)
	}
	if kind := tsSym(t, ff, "js.legacy").Props["symbol_kind"]; kind != facts.SymbolFunc {
		t.Errorf("legacy is a %v, want a function: its value is one", kind)
	}
}

// A class declared in a wrapper keeps its fields: they are the class's variables, not
// the wrapper's.
func TestWrapperKeepsClassFields(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{"js/serve.js": `(function () {
  var limit = 3;
  class Context {
    closed;
    listener = null;
    close() { this.closed = true; }
  }
  new Context().close();
})();`})
	got := strings.Join(symbolsIn(ff, "js/serve.js"), ",")
	if got != "js.Context,js.Context.close,js.Context.closed,js.Context.listener" {
		t.Errorf("symbols = %q, want the class with its method and both fields", got)
	}
}

// Outside a wrapper nothing is dropped, and a function written as a variable's value is
// a function there too.
func TestModuleVariablesAreKept(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{"src/mod.ts": "export const LIMIT = 3\nconst helper = function () { return LIMIT }\n"})
	if got := strings.Join(symbolsIn(ff, "src/mod.ts"), ","); got != "src.LIMIT,src.helper" {
		t.Errorf("symbols = %q, want src.LIMIT,src.helper", got)
	}
	if kind := tsSym(t, ff, "src.helper").Props["symbol_kind"]; kind != facts.SymbolFunc {
		t.Errorf("helper is a %v, want a function", kind)
	}
}
