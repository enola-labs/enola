package swiftextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// propModuleHead marks, during extraction only, a declaration in an extension
// whose written path starts with a name that may be a module
// (`extension KsApi.Category`). dropModuleHeads removes it.
const propModuleHead = "swift_module_head"

// dropModuleHeads names a declaration of an extension written through a module
// (`extension Foundation.URLSessionWebSocketTask.Message`) after the path past the
// module, as the extension of the bare path names it, unless the repository
// declares a top-level type of the module's name: Swift then reads the head as that
// type (`extension AudioPlayerClient.Sound` in a module of the same name). Edges
// the file's walk drew to the old name follow it.
func dropModuleHeads(perFile [][]facts.Fact, moduleOf func(string) string) {
	topLevel := map[string]bool{}
	for _, ff := range perFile {
		for i := range ff {
			f := &ff[i]
			if f.Kind != facts.KindSymbol {
				continue
			}
			switch sk, _ := f.PropAny("symbol_kind").(string); sk {
			case facts.SymbolStruct, facts.SymbolClass, facts.SymbolInterface, facts.SymbolType:
			default:
				continue
			}
			dir := moduleOf(f.File)
			if rest, ok := strings.CutPrefix(f.Name, dir+"."); ok && !strings.Contains(rest, ".") {
				topLevel[rest] = true
			}
		}
	}
	for _, ff := range perFile {
		renamed := map[string]string{}
		for i := range ff {
			f := &ff[i]
			head, _ := f.PropAny(propModuleHead).(string)
			if head == "" {
				continue
			}
			delete(f.Props, propModuleHead)
			if topLevel[head] {
				continue
			}
			dir := moduleOf(f.File)
			old := f.Name
			if rest, ok := strings.CutPrefix(old, dir+"."+head+"."); ok {
				f.Name = dir + "." + rest
				renamed[old] = f.Name
				if recv, _ := f.PropAny("receiver").(string); strings.HasPrefix(recv, head+".") {
					f.SetProp("receiver", recv[len(head)+1:])
				}
			}
		}
		if len(renamed) == 0 {
			continue
		}
		for i := range ff {
			for j, r := range ff[i].Relations {
				if n, ok := renamed[r.Target]; ok {
					ff[i].Relations[j].Target = n
				}
			}
		}
	}
}
