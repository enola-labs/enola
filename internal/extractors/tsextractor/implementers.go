package tsextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

func declaredImplementers(all []facts.Fact) map[string][]string {
	byShort := make(map[string][]string)
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		kind, _ := f.PropAny("symbol_kind").(string)
		abstract, _ := f.PropAny("abstract").(bool)
		if kind != facts.SymbolInterface && !abstract {
			continue
		}
		short := f.Name[strings.LastIndexByte(f.Name, '.')+1:]
		byShort[short] = append(byShort[short], f.Name)
	}
	out := make(map[string][]string)
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelImplements {
				continue
			}
			short := r.Target[strings.LastIndexByte(r.Target, '.')+1:]
			if ifaces := byShort[short]; len(ifaces) == 1 && ifaces[0] != f.Name {
				out[ifaces[0]] = append(out[ifaces[0]], f.Name)
			}
		}
	}
	return out
}
