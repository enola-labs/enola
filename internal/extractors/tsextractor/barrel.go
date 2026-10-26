package tsextractor

import (
	"path"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

var (
	barrelExportFromRe = regexp.MustCompile(`export\s*\{([^}]*)\}\s*from\s*['"]([^'"]+)['"]`)
	barrelExportRe     = regexp.MustCompile(`export\s*\{([^}]*)\}\s*;?\s*(?:\n|$)`)
	barrelDefaultImpRe = regexp.MustCompile(`import\s+([A-Za-z_$][\w$]*)\s+from\s*['"]([^'"]+)['"]`)
	barrelNamedImpRe   = regexp.MustCompile(`import\s*\{([^}]*)\}\s*from\s*['"]([^'"]+)['"]`)
)

// barrelExports maps "<folder>.<Name>" to the symbol a folder's index file
// re-exports under that name: `export { A } from './a'`, or a binding imported and
// then exported (`import V from './v.vue'; export { V }`). An import of the folder
// binds to "<folder>.<Name>", which no fact declares, so without this a barrel
// leaves every import through it dangling.
func barrelExports(tsFiles []string, sources map[string][]byte, knownFiles map[string]bool) map[string]string {
	out := map[string]string{}
	for _, file := range tsFiles {
		if strings.TrimSuffix(path.Base(file), path.Ext(file)) != "index" || facts.IsTestPath(file) {
			continue
		}
		src := string(sources[file])
		if !strings.Contains(src, "export") {
			continue
		}
		folder := factpath.Dir(file)
		target := func(spec, name string) string {
			if !strings.HasPrefix(spec, ".") {
				return ""
			}
			idx, dir, ok := resolveModuleFile(factpath.Clean(path.Join(folder, spec)), knownFiles)
			if !ok {
				return ""
			}
			if name == "default" {
				return dir + "." + fileSymbolName(idx)
			}
			return dir + "." + name
		}
		for _, m := range barrelExportFromRe.FindAllStringSubmatch(src, -1) {
			for _, spec := range splitSpecifiers(m[1]) {
				if t := target(m[2], spec[0]); t != "" {
					out[folder+"."+spec[1]] = t
				}
			}
		}
		imported := map[string]string{}
		for _, m := range barrelDefaultImpRe.FindAllStringSubmatch(src, -1) {
			if t := target(m[2], "default"); t != "" {
				imported[m[1]] = t
			}
		}
		for _, m := range barrelNamedImpRe.FindAllStringSubmatch(src, -1) {
			for _, spec := range splitSpecifiers(m[1]) {
				if t := target(m[2], spec[0]); t != "" {
					imported[spec[1]] = t
				}
			}
		}
		for _, m := range barrelExportRe.FindAllStringSubmatch(src, -1) {
			for _, spec := range splitSpecifiers(m[1]) {
				if t := imported[spec[0]]; t != "" {
					out[folder+"."+spec[1]] = t
				}
			}
		}
	}
	return out
}

// splitSpecifiers reads `A, B as C, type D` into {name, alias} pairs.
func splitSpecifiers(list string) [][2]string {
	var out [][2]string
	for _, part := range strings.Split(list, ",") {
		fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(part), "type "))
		switch {
		case len(fields) == 1:
			out = append(out, [2]string{fields[0], fields[0]})
		case len(fields) == 3 && fields[1] == "as":
			out = append(out, [2]string{fields[0], fields[2]})
		}
	}
	return out
}

// resolveBarrelCalls rewrites a dangling call target to the symbol a barrel
// re-exports under that name, when that symbol exists.
func resolveBarrelCalls(all []facts.Fact, barrels map[string]string) {
	if len(barrels) == 0 {
		return
	}
	exists := make(map[string]bool, len(all))
	for _, f := range all {
		if f.Kind == facts.KindSymbol {
			exists[f.Name] = true
		}
	}
	for i := range all {
		for j := range all[i].Relations {
			r := &all[i].Relations[j]
			if r.Kind != facts.RelCalls || exists[r.Target] {
				continue
			}
			if t := barrels[r.Target]; t != "" && exists[t] {
				r.Target = t
			}
		}
	}
}
