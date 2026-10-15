package tsextractor

import (
	"strings"
	"testing"
)

// A setter and a getter are one name. Reading the property is the getter's body,
// not the setter's; a setter with no getter still hands its I/O to an assignment.
func TestAccessor_SetterIOIsNotTheGetters(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{
		"src/layout.ts": `export class Layout {
  private ctxValue: object;
  private styleValue: string;

  set ctx(val: object) {
    this.ctxValue = val;
    this.loadStyle();
  }

  get ctx(): object {
    return this.ctxValue;
  }

  set style(val: string) {
    fetch('/style/' + val);
  }

  loadStyle() {
    return fetch('/style');
  }

  onMouseDown() {
    return this.ctx;
  }

  restyle(val: string) {
    this.style = val;
  }
}
`,
	})
	var setter, getter bool
	for _, f := range ff {
		if f.Kind != "symbol" || !strings.HasSuffix(f.Name, "Layout.ctx") {
			continue
		}
		if f.PropAny("symbol_kind") == "getter" {
			getter = tsBool(f, "performs_io")
		} else if f.PropAny(propAccessor) == "set" {
			setter = tsBool(f, "performs_io")
		}
	}
	if !setter || getter {
		t.Errorf("ctx: setter performs_io = %v, getter = %v; want true, false", setter, getter)
	}
	if tsBool(tsSym(t, ff, "Layout.onMouseDown"), "performs_io") {
		t.Errorf("reading ctx was answered by the setter's body")
	}
	if !tsBool(tsSym(t, ff, "Layout.style"), "performs_io") {
		t.Errorf("a setter with no getter lost its I/O")
	}
}
