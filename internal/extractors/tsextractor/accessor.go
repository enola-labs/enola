package tsextractor

import "github.com/enola-labs/enola/internal/facts"

// propAccessor is "set" on a setter. A getter is told by its symbol_kind.
const propAccessor = "accessor"

// settersBesideGetters reports the setters that share their name with a getter.
// Facts are keyed by name, so without it a read of the property is answered by
// the setter's body: `set ctx(v) { this.load() }` made every `this.ctx` read I/O.
// A setter with no getter keeps its name, so an assignment still reaches its I/O.
func settersBesideGetters(all []facts.Fact) func(facts.Fact) bool {
	getters := make(map[string]bool)
	for _, f := range all {
		if f.Kind == facts.KindSymbol {
			if kind, _ := f.PropAny("symbol_kind").(string); kind == facts.SymbolGetter {
				getters[f.Name] = true
			}
		}
	}
	return func(f facts.Fact) bool {
		accessor, _ := f.PropAny(propAccessor).(string)
		return accessor == "set" && getters[f.Name]
	}
}
