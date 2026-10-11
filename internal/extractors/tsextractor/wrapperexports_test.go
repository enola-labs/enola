package tsextractor

import (
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// exportedIn returns the short names of the symbols a file declares as exported, sorted.
func exportedIn(ff []facts.Fact, file string) string {
	var out []string
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.File == file && f.Props["exported"] == true {
			out = append(out, f.Name[strings.Index(f.Name, ".")+1:])
		}
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}

// What a wrapper hands out is exported in everything but syntax. Read as unexported, a
// function whose only use was being handed out was a dead-code candidate of the kind
// reported as safest to delete.
func TestWrapperExposedDeclarationsAreExported(t *testing.T) {
	const decls = `
  function roll() {}
  function helper() {}
  class Game { start() {} }
  const codes = new Map();
  const secret = 1;
`
	cases := []struct{ name, src, exported, symbols string }{
		{
			"the object the wrapper returns, by shorthand and by rename",
			"(function () {" + decls + "  return { roll, Game, table: codes };\n})();",
			"Game,Game.start,codes,roll", "js.Game,js.Game.start,js.codes,js.helper,js.roll",
		},
		{
			"a single returned value",
			"(function () {" + decls + "  return Game;\n})();",
			"Game,Game.start", "js.Game,js.Game.start,js.helper,js.roll",
		},
		{
			"a property of the global object",
			"(function () {" + decls + "  window.Game = Game;\n  globalThis.dice = { roll: roll };\n})();",
			"Game,Game.start,roll", "js.Game,js.Game.start,js.helper,js.roll",
		},
		{
			"the global object under the name the wrapper was passed it as",
			"(function (root, $) {" + decls + "  root.app.Game = Game;\n  $.roll = roll;\n})(this, jQuery);",
			"Game,Game.start", "js.Game,js.Game.start,js.helper,js.roll",
		},
		{
			"the CommonJS module",
			"(function () {" + decls + "  module.exports = { roll };\n  exports.codes = codes;\n})();",
			"codes,roll", "js.Game,js.Game.start,js.codes,js.helper,js.roll",
		},
	}
	for _, tc := range cases {
		ff := extractTSRepo(t, map[string]string{"js/game.js": tc.src, "js/keep.ts": "export const x = 1\n"})
		if got := exportedIn(ff, "js/game.js"); got != tc.exported {
			t.Errorf("%s: exported = %q, want %q", tc.name, got, tc.exported)
		}
		if got := strings.Join(symbolsIn(ff, "js/game.js"), ","); got != tc.symbols {
			t.Errorf("%s: symbols = %q, want %q", tc.name, got, tc.symbols)
		}
	}
}

// Only the wrapper's own result and its assignments to the outside expose anything.
func TestWrapperKeepsPrivateWhatItDoesNotHandOut(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{"js/app.js": `(function ($) {
  function early() {}
  function nested() { return { early }; }
  function namespaced() {}
  function passed() {}
  function local() {}
  if (window.debug) { return; }
  wp.media = namespaced;
  $.fn.plugin = passed;
  var holder = {};
  holder.local = local;
  window.inline = function () {};
  return { missing, renamed: 42 };
})(jQuery);`, "js/keep.ts": "export const x = 1\n"})
	if got := exportedIn(ff, "js/app.js"); got != "" {
		t.Errorf("exported = %q, want none: a namespace, a library object and a local object are not the outside", got)
	}
}
