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
// at once. Read as that function's locals, the file had no symbols at all.
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
		"a factory handed to a universal-module wrapper": `(function (root, factory) {
  if (typeof module === 'object') { module.exports = factory(); } else { root.Game = factory(); }
})(this, function () {
  const SIDES = 6;
  function roll() { return SIDES; }
  class Game { start() { return roll(); } }
  const pick = (xs) => xs[0];
  return Game;
});`,
	}
	want := "js.Game,js.Game.start,js.SIDES,js.pick,js.roll"
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
