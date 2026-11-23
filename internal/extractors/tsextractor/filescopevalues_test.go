package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// fileRefNames returns the `names` targets of the file_ref fact of a file.
func fileRefNames(ff []facts.Fact, file string) []string {
	var out []string
	for _, f := range ff {
		if f.Kind != facts.KindFileRef || f.File != file {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames {
				out = append(out, r.Target)
			}
		}
	}
	return out
}

// A script that keeps its code in a function it calls at once hands its functions on
// by value from that function's body: a ternary branch, the right side of an
// assignment to a global. Those are uses, and so is a sibling function naming one.
func TestFileScopeValues_WrapperBody(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/polyfill.js": `;(function () {
  var global = window
  function encode (s) { return s }
  function decode (s) { return s }
  function BlobCtor () {}
  function OnlyDecorated () {}
  function pick () { return typeof TextDecoder === 'function' ? null : decode }
  var textEncode = typeof TextEncoder === 'function' ? null : encode
  if (global.Blob) { OnlyDecorated.prototype = Blob.prototype }
  global.Blob = BlobCtor
})()
`,
	}, false)
	names := fileRefNames(ff, "src/polyfill.js")
	for _, want := range []string{"src.encode", "src.BlobCtor"} {
		if !hasTarget(names, want) {
			t.Errorf("the wrapper's body hands %s on by value; file names %v", want, names)
		}
	}
	if hasTarget(names, "src.OnlyDecorated") {
		t.Errorf("assigning a property of a function is no use of it: %v", names)
	}
	pick, _ := findFact(ff, "src.pick")
	if !hasRelation(pick, facts.RelNames, "src.decode") {
		t.Errorf("a wrapper's function naming a sibling by value draws no use: %v", pick.Relations)
	}
}

// At a module's top level too, an expression statement that hands a function on is
// a use of it; a property set on a declaration (`Foo.propTypes = ...`) is not.
func TestFileScopeValues_TopLevelStatements(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/top.js": `function BlobCtor () {}
function Card () { return null }
function init () {}
Card.propTypes = {}
window.Blob = BlobCtor
document.addEventListener('load', init)
`,
	}, false)
	names := fileRefNames(ff, "src/top.js")
	if !hasTarget(names, "src.BlobCtor") {
		t.Errorf("assigning a function to a global is a use of it; file names %v", names)
	}
	if hasTarget(names, "src.Card") {
		t.Errorf("setting a property on a component is no use of it: %v", names)
	}
	if hasTarget(names, "src.init") {
		t.Errorf("a call argument stays a call reference, not also a name: %v", names)
	}
	if !hasTarget(fileRefTargets(ff, "src/top.js"), "src.init") {
		t.Errorf("a call argument is still referenced: %v", fileRefTargets(ff, "src/top.js"))
	}
}

// An imported name is the file's reference wherever it is written already; the
// file-scope walk adds no second edge to it.
func TestFileScopeValues_ImportsStayCalls(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/index.js":  "import config from './config'\nwindow.config = config\n",
		"src/config.js": "export default {}\n",
	}, false)
	if names := fileRefNames(ff, "src/index.js"); len(names) != 0 {
		t.Errorf("an imported value is named twice: %v", names)
	}
	if !hasTarget(fileRefTargets(ff, "src/index.js"), "src.config") {
		t.Errorf("the imported value is no longer referenced: %v", fileRefTargets(ff, "src/index.js"))
	}
}
